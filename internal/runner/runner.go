// Package runner implements the per-machine daemon that drives Claude Code.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"multiclaude/internal/proto"
)

type Config struct {
	Server         string
	Name           string
	Roles          []string
	ExtraCaps      []string
	Concurrency    int
	ClaudePath     string
	WorkDir        string
	PermissionMode string
	ExtraArgs      []string
	JoinToken      string // enrolls the machine if it has no credentials yet
}

// credentials are what a machine keeps after enrolling (WorkDir/credentials.json).
type credentials struct {
	Server string `json:"server"`
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

type Runner struct {
	cfg    Config
	nodeID string
	token  string // node token (Bearer)
	http   *http.Client
	self   string // path of this executable (used as MCP server command)

	mu       sync.Mutex
	sessions map[string]*session
	wsInUse  map[string]bool
}

func defaultWorkDir(dir string) string {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".multiclaude")
	}
	return dir
}

func loadCredentials(workDir string) credentials {
	var c credentials
	if b, err := os.ReadFile(filepath.Join(defaultWorkDir(workDir), "credentials.json")); err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func New(cfg Config) (*Runner, error) {
	cfg.WorkDir = defaultWorkDir(cfg.WorkDir)
	if abs, err := filepath.Abs(cfg.WorkDir); err == nil {
		cfg.WorkDir = abs // claude runs in the checkout, so paths must not be relative
	}
	creds := loadCredentials(cfg.WorkDir)
	if cfg.Server == "" {
		cfg.Server = creds.Server
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Server == "" {
		return nil, errors.New("--server is required")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, err
	}
	cfg.ClaudePath = FindClaude(cfg.ClaudePath)
	if cfg.ClaudePath == "" {
		return nil, errors.New("could not find the `claude` CLI; install Claude Code or pass --claude /path/to/claude")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, self: self, http: &http.Client{Timeout: 60 * time.Second},
		sessions: map[string]*session{}, wsInUse: map[string]bool{}}
	if b, err := os.ReadFile(filepath.Join(cfg.WorkDir, "node-id")); err == nil {
		r.nodeID = strings.TrimSpace(string(b)) // pre-auth versions
	}
	if creds.Token != "" {
		r.nodeID, r.token = creds.NodeID, creds.Token
	}
	return r, nil
}

func (r *Runner) saveCredentials() error {
	b, _ := json.MarshalIndent(credentials{Server: r.cfg.Server, NodeID: r.nodeID, Token: r.token}, "", "  ")
	return os.WriteFile(filepath.Join(r.cfg.WorkDir, "credentials.json"), b, 0o600)
}

// gitEnv makes git (and git-lfs) send the node token to the MultiClaude server
// only, without writing it into any repository config.
func (r *Runner) gitEnv() []string {
	if r.token == "" {
		return nil
	}
	return []string{"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + r.cfg.Server + "/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + r.token}
}

func (r *Runner) post(path string, in, out any) error {
	return r.postCtx(context.Background(), path, in, out)
}

func (r *Runner) postCtx(ctx context.Context, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", r.cfg.Server+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return &httpError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

func (r *Runner) RegisterRequest() proto.RegisterRequest {
	hw := DetectHardware(r.cfg.WorkDir)
	name := r.cfg.Name
	if name == "" {
		name = hw.Hostname
	}
	return proto.RegisterRequest{
		NodeID:         r.nodeID,
		Name:           name,
		Hardware:       hw,
		Capabilities:   Capabilities(hw, r.cfg.ExtraCaps),
		Roles:          r.cfg.Roles,
		MaxConcurrency: r.cfg.Concurrency,
		ClaudePath:     r.cfg.ClaudePath,
		ClaudeVersion:  cmdOut(r.cfg.ClaudePath, "--version"),
		ClaudeAuth:     ClaudeAuth(),
		RunnerVersion:  proto.Version,
	}
}

func (r *Runner) register() error {
	req := r.RegisterRequest()
	if r.token == "" {
		if r.cfg.JoinToken == "" {
			return errors.New("this machine is not enrolled yet: run `multiclaude-runner enroll --server URL --token <join token>` (the command is shown under Machines → Add a machine)")
		}
		req.JoinToken = r.cfg.JoinToken
	}
	var resp proto.RegisterResponse
	err := r.post("/api/runner/register", req, &resp)
	var he *httpError
	if errors.As(err, &he) && he.code == 401 && r.token != "" && r.cfg.JoinToken != "" {
		log.Printf("machine token rejected; enrolling again with the join token")
		r.token, req.JoinToken = "", r.cfg.JoinToken
		err = r.post("/api/runner/register", req, &resp)
	}
	if err != nil {
		return err
	}
	r.nodeID = resp.NodeID
	if resp.NodeToken != "" {
		r.token = resp.NodeToken
		if err := r.saveCredentials(); err != nil {
			return fmt.Errorf("saving credentials: %w", err)
		}
		os.Remove(filepath.Join(r.cfg.WorkDir, "node-id"))
		log.Printf("enrolled with %s; credentials saved to %s", r.cfg.Server, filepath.Join(r.cfg.WorkDir, "credentials.json"))
	}
	log.Printf("registered as %q (%s) caps=%v roles=%v claude=%s (%s)", req.Name, r.nodeID, req.Capabilities, req.Roles, req.ClaudeVersion, req.ClaudeAuth)
	return nil
}

// Run is the main loop.
func (r *Runner) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := r.register(); err != nil {
			log.Printf("register failed: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if err := r.pollLoop(ctx); err != nil {
			log.Printf("poll loop: %v", err)
		}
		if ctx.Err() != nil {
			r.shutdown()
			return nil
		}
	}
}

// pollLoop returns when the server forgot us (re-register needed).
func (r *Runner) pollLoop(ctx context.Context) error {
	fails := 0
	for ctx.Err() == nil {
		r.mu.Lock()
		active := make([]string, 0, len(r.sessions))
		for id := range r.sessions {
			active = append(active, id)
		}
		r.mu.Unlock()
		var resp proto.PollResponse
		err := r.postCtx(ctx, "/api/runner/poll", proto.PollRequest{NodeID: r.nodeID, FreeSlots: r.cfg.Concurrency - len(active), ActiveRuns: active}, &resp)
		if err != nil {
			var he *httpError
			if errors.As(err, &he) && (he.code == 404 || he.code == 401) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			fails++
			if fails%10 == 1 {
				log.Printf("poll error: %v", err)
			}
			time.Sleep(3 * time.Second)
			continue
		}
		fails = 0
		for _, c := range resp.Commands {
			r.handle(c)
		}
	}
	return nil
}

func (r *Runner) handle(c proto.Command) {
	switch c.Type {
	case "assign":
		if c.Assignment == nil {
			return
		}
		s := newSession(r, c.Assignment)
		r.mu.Lock()
		r.sessions[s.a.RunID] = s
		r.mu.Unlock()
		go func() {
			fin := s.run()
			r.mu.Lock()
			delete(r.sessions, s.a.RunID)
			r.mu.Unlock()
			for i := 0; i < 20; i++ {
				if err := r.post("/api/runner/runs/"+s.a.RunID+"/finish", fin, nil); err == nil {
					break
				}
				time.Sleep(3 * time.Second)
			}
		}()
	case "message":
		r.mu.Lock()
		s := r.sessions[c.RunID]
		r.mu.Unlock()
		if s != nil {
			s.sendUser(c.Message)
		}
	case "cancel":
		r.mu.Lock()
		s := r.sessions[c.RunID]
		r.mu.Unlock()
		if s != nil {
			s.cancel()
		}
	}
}

func (r *Runner) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		s.cancel()
	}
}

// acquireWorkspace returns a reusable checkout directory for a project.
func (r *Runner) acquireWorkspace(slug string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 1; ; i++ {
		dir := filepath.Join(r.cfg.WorkDir, "workspaces", slug, fmt.Sprintf("slot-%d", i))
		if !r.wsInUse[dir] {
			r.wsInUse[dir] = true
			return dir
		}
	}
}

func (r *Runner) releaseWorkspace(dir string) {
	r.mu.Lock()
	delete(r.wsInUse, dir)
	r.mu.Unlock()
}

// Enroll registers this machine with a join token and stores its credentials.
func Enroll(cfg Config) error {
	if cfg.JoinToken == "" {
		return errors.New("--token is required (copy it from Machines → Add a machine)")
	}
	r, err := New(cfg)
	if err != nil {
		return err
	}
	if err := r.register(); err != nil {
		return err
	}
	fmt.Printf("Enrolled %s as machine %s. Start it with: multiclaude-runner run\n", r.cfg.Server, r.nodeID)
	return nil
}

// Doctor prints what the runner would report.
func Doctor(cfg Config) {
	fmt.Println("MultiClaude runner", proto.Version)
	r, err := New(cfg)
	if err != nil {
		fmt.Println("ERROR:", err)
		return
	}
	req := r.RegisterRequest()
	b, _ := json.MarshalIndent(req, "", "  ")
	fmt.Println(string(b))
	for _, t := range []string{"git", "git-lfs"} {
		if _, err := exec.LookPath(t); err != nil {
			fmt.Printf("WARNING: %s not found in PATH\n", t)
		}
	}
	if os.Geteuid() == 0 && cfg.PermissionMode == "bypassPermissions" {
		fmt.Println("WARNING: Claude Code refuses bypassPermissions as root; run the runner as a normal user")
	}
	resp, err := http.Get(r.cfg.Server + "/api/auth")
	if err != nil {
		fmt.Println("WARNING: server not reachable:", err)
		return
	}
	resp.Body.Close()
	fmt.Println("Server reachable:", r.cfg.Server)
	if r.token == "" {
		fmt.Println("Not enrolled yet: run `multiclaude-runner enroll --server", r.cfg.Server, "--token <join token>`")
	} else {
		fmt.Println("Enrolled as machine", r.nodeID)
	}
}
