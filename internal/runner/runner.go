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
}

type Runner struct {
	cfg    Config
	nodeID string
	http   *http.Client
	self   string // path of this executable (used as MCP server command)

	mu       sync.Mutex
	sessions map[string]*session
	wsInUse  map[string]bool
}

func New(cfg Config) (*Runner, error) {
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Server == "" {
		return nil, errors.New("--server is required")
	}
	if cfg.WorkDir == "" {
		home, _ := os.UserHomeDir()
		cfg.WorkDir = filepath.Join(home, ".multiclaude")
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
		r.nodeID = strings.TrimSpace(string(b))
	}
	return r, nil
}

func (r *Runner) post(path string, in, out any) error {
	b, _ := json.Marshal(in)
	resp, err := r.http.Post(r.cfg.Server+path, "application/json", bytes.NewReader(b))
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
	var resp proto.RegisterResponse
	if err := r.post("/api/runner/register", req, &resp); err != nil {
		return err
	}
	r.nodeID = resp.NodeID
	os.WriteFile(filepath.Join(r.cfg.WorkDir, "node-id"), []byte(r.nodeID), 0o644)
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
		err := r.post("/api/runner/poll", proto.PollRequest{NodeID: r.nodeID, FreeSlots: r.cfg.Concurrency - len(active), ActiveRuns: active}, &resp)
		if err != nil {
			var he *httpError
			if errors.As(err, &he) && he.code == 404 {
				return err
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
	resp, err := http.Get(r.cfg.Server + "/api/overview")
	if err != nil {
		fmt.Println("WARNING: server not reachable:", err)
		return
	}
	resp.Body.Close()
	fmt.Println("Server reachable:", r.cfg.Server)
}
