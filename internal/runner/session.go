package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"multiclaude/internal/proto"
)

// session is one assignment: workspace preparation, one or more Claude Code
// processes (stream-json in/out) and the final push.
type session struct {
	r *Runner
	a *proto.Assignment

	mu        sync.Mutex
	stdin     io.WriteCloser
	pending   int      // user messages sent that have no result yet
	leftover  []string // messages that arrived after stdin was closed
	cancelled bool
	proc      *exec.Cmd

	sessionID string
	result    string
	isError   bool
	cost      float64
	stderr    bytes.Buffer

	events chan proto.RunEvent
	dir    string
	repos  []wsRepo
}

func newSession(r *Runner, a *proto.Assignment) *session {
	return &session{r: r, a: a, events: make(chan proto.RunEvent, 4096), sessionID: a.ResumeSession}
}

func (s *session) emit(typ string, data any) {
	var raw json.RawMessage
	switch v := data.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	default:
		raw, _ = json.Marshal(v)
	}
	if len(raw) > 150_000 {
		raw, _ = json.Marshal(map[string]any{"type": typ, "truncated": true, "preview": string(raw[:20_000])})
	}
	select {
	case s.events <- proto.RunEvent{Type: typ, Data: raw}:
	default:
	}
}

func (s *session) info(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("[%s] %s", s.a.RunID, msg)
	s.emit("runner", map[string]string{"type": "runner", "text": msg})
}

// shipEvents batches events to the server.
func (s *session) shipEvents(done <-chan struct{}) {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	var buf []proto.RunEvent
	flush := func() {
		if len(buf) == 0 {
			return
		}
		for i := 0; i < 5; i++ {
			if err := s.r.post("/api/runner/runs/"+s.a.RunID+"/events", buf, nil); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		buf = buf[:0]
	}
	for {
		select {
		case e := <-s.events:
			buf = append(buf, e)
			if len(buf) >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		case <-done:
			for {
				select {
				case e := <-s.events:
					buf = append(buf, e)
				default:
					flush()
					return
				}
			}
		}
	}
}

// wsRepo is one repository checked out in the workspace.
type wsRepo struct {
	spec   proto.RepoSpec
	dir    string
	branch string // branch this session works on in this repo
}

func (s *session) gitIn(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "SSH_ASKPASS="), s.r.gitEnv()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// repoSpecs returns the repositories of the assignment (one for older servers).
func (s *session) repoSpecs() []proto.RepoSpec {
	if len(s.a.Repos) > 0 {
		return s.a.Repos
	}
	return []proto.RepoSpec{{Name: s.a.ProjectSlug, URL: s.a.RepoURL, BaseBranch: s.a.BaseBranch}}
}

func (s *session) multiRepo() bool {
	sp := s.repoSpecs()
	return len(sp) > 1 || sp[0].Dir != ""
}

// environmentPrompt adds the facts only the runner knows to the system prompt.
func (s *session) environmentPrompt() string {
	var b strings.Builder
	b.WriteString("\nEnvironment:\n")
	if !s.multiRepo() {
		fmt.Fprintf(&b, "- Git remote (origin): %s\n- Local checkout (your working directory): %s\n", s.r.cfg.Server+s.a.RepoURL, s.dir)
	} else {
		fmt.Fprintf(&b, "- Your working directory %s holds one checkout per repository of the project:\n", s.dir)
		for _, rp := range s.repos {
			fmt.Fprintf(&b, "    %s/  → origin %s (branch %s, default branch %s)\n", rp.spec.Dir, s.r.cfg.Server+rp.spec.URL, rp.branch, rp.spec.BaseBranch)
		}
		b.WriteString("  The workspace root itself is not a git repository: run git inside the repo directories (git -C <dir> …).\n")
	}
	fmt.Fprintf(&b, "- MultiClaude server: %s (board: %s/p/%s)\n- Current branch: %s\n", s.r.cfg.Server, s.r.cfg.Server, s.a.ProjectSlug, s.a.Branch)
	b.WriteString("- Git authentication to the MultiClaude remotes is already configured through GIT_CONFIG_* environment variables; just use git normally. Never print, log or commit those values.\n")
	return b.String()
}

func hasLFS() bool { _, err := exec.LookPath("git-lfs"); return err == nil }

func (s *session) prepare() error {
	if s.multiRepo() {
		os.MkdirAll(s.dir, 0o755)
	}
	s.repos = nil
	for _, sp := range s.repoSpecs() {
		rp := wsRepo{spec: sp, dir: filepath.Join(s.dir, sp.Dir), branch: s.a.Branch}
		if s.a.Kind == "chat" || s.a.Branch == s.a.BaseBranch {
			rp.branch = sp.BaseBranch // no issue branch: every repo on its own default branch
		}
		if err := s.prepareRepo(rp); err != nil {
			if len(s.repoSpecs()) > 1 {
				return fmt.Errorf("repository %s: %w", sp.Name, err)
			}
			return err
		}
		s.repos = append(s.repos, rp)
	}
	return nil
}

func (s *session) prepareRepo(rp wsRepo) error {
	url := s.r.cfg.Server + rp.spec.URL
	g := func(args ...string) (string, error) { return s.gitIn(rp.dir, args...) }
	if _, err := os.Stat(filepath.Join(rp.dir, ".git")); err != nil {
		os.MkdirAll(filepath.Dir(rp.dir), 0o755)
		os.RemoveAll(rp.dir)
		s.info("cloning %s", url)
		if _, err := s.gitIn(filepath.Dir(rp.dir), "clone", url, rp.dir); err != nil {
			return fmt.Errorf("clone failed: %v", err)
		}
		if hasLFS() {
			g("lfs", "install", "--local")
		}
	}
	g("remote", "set-url", "origin", url)
	name := s.r.cfg.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	g("config", "user.name", name+" (Claude via MultiClaude)")
	g("config", "user.email", strings.ReplaceAll(strings.ToLower(name), " ", "-")+"@multiclaude.local")
	if _, err := g("fetch", "origin", "--prune"); err != nil {
		return err
	}
	g("merge", "--abort")
	g("rebase", "--abort")
	g("reset", "--hard")
	g("clean", "-fd")
	start := "origin/" + rp.spec.BaseBranch
	if _, err := g("rev-parse", "--verify", "refs/remotes/origin/"+rp.branch); err == nil {
		start = "origin/" + rp.branch
	}
	if _, err := g("checkout", "-B", rp.branch, start); err != nil {
		return err
	}
	if hasLFS() {
		if _, err := g("lfs", "pull"); err != nil {
			s.info("git lfs pull failed: %v", err)
		}
	}
	s.info("workspace %s on %s (from %s)", rp.dir, rp.branch, start)
	return nil
}

// finalizeGit pushes the branch in every repo that has new commits. With
// salvage (run failed or was cancelled) uncommitted changes on an issue branch
// are committed as WIP so the next attempt can continue; after successful runs
// leftovers are usually build artifacts and are left out (the next run starts
// from a clean checkout).
func (s *session) finalizeGit(salvage bool) (bool, string) {
	pushedAny, head := false, ""
	for i, rp := range s.repos {
		p, h := s.finalizeRepo(rp, salvage)
		pushedAny = pushedAny || p
		if i == 0 {
			head = h
		}
	}
	return pushedAny, head
}

func (s *session) finalizeRepo(rp wsRepo, salvage bool) (bool, string) {
	g := func(args ...string) (string, error) { return s.gitIn(rp.dir, args...) }
	label := rp.branch
	if len(s.repos) > 1 {
		label = rp.spec.Name + ":" + rp.branch
	}
	onIssueBranch := rp.branch != rp.spec.BaseBranch
	if st, _ := g("status", "--porcelain"); st != "" {
		if salvage && onIssueBranch && s.a.Kind == "work" {
			g("add", "-A")
			if _, err := g("commit", "-m", fmt.Sprintf("WIP: uncommitted changes salvaged from agent run %s", s.a.RunID)); err == nil {
				s.info("%s: salvaged uncommitted changes into a WIP commit", label)
			}
		} else {
			s.info("%s: leaving uncommitted changes out of the push:\n%s", label, lastN(st, 1500))
		}
	}
	head, _ := g("rev-parse", "HEAD")
	remote := "refs/remotes/origin/" + rp.branch
	ahead := "1"
	if _, err := g("rev-parse", "--verify", remote); err == nil {
		ahead, _ = g("rev-list", "--count", remote+"..HEAD")
	} else if base, err := g("rev-parse", "origin/"+rp.spec.BaseBranch); err == nil && base == head {
		ahead = "0"
	}
	if ahead == "0" || ahead == "" {
		return false, head
	}
	push := func() error {
		_, err := g("push", "origin", "HEAD:refs/heads/"+rp.branch)
		return err
	}
	if err := push(); err != nil {
		s.info("%s: push rejected, integrating remote changes: %v", label, err)
		if _, err := g("pull", "--rebase", "origin", rp.branch); err != nil {
			g("rebase", "--abort")
			if _, err := g("pull", "--no-rebase", "--no-edit", "origin", rp.branch); err != nil {
				g("merge", "--abort")
				s.info("%s: could not integrate remote changes: %v", label, err)
				return false, head
			}
		}
		if err := push(); err != nil {
			s.info("%s: push failed: %v", label, err)
			return false, head
		}
	}
	head, _ = g("rev-parse", "HEAD")
	s.info("pushed %s (%s)", label, head[:min(8, len(head))])
	return true, head
}

func (s *session) run() proto.RunFinish {
	done := make(chan struct{})
	shipped := make(chan struct{})
	go func() { s.shipEvents(done); close(shipped) }()
	defer func() { close(done); <-shipped }()

	wsKey := s.a.ProjectSlug
	if s.multiRepo() {
		wsKey += "~multi" // different layout: one subdirectory per repository
	}
	s.dir = s.r.acquireWorkspace(wsKey)
	defer s.r.releaseWorkspace(s.dir)
	s.info("starting %s run in project %s", s.a.Kind, s.a.ProjectSlug)

	if err := s.prepare(); err != nil {
		s.info("workspace preparation failed: %v", err)
		return proto.RunFinish{Status: "failed", Error: err.Error()}
	}

	messages := []string{s.a.Prompt}
	var runErr error
	for len(messages) > 0 && !s.isCancelled() {
		runErr = s.runClaude(messages)
		if runErr != nil && s.a.ResumeSession != "" && s.result == "" && !s.isCancelled() {
			// The session lives in another workspace dir (or expired): start fresh.
			s.info("resuming session %s failed, starting a new session", s.a.ResumeSession)
			s.a.ResumeSession, s.sessionID = "", ""
			runErr = s.runClaude(messages)
		}
		s.mu.Lock()
		messages = s.leftover
		s.leftover = nil
		s.mu.Unlock()
		if runErr != nil || s.sessionID == "" {
			break
		}
	}

	pushed, head := s.finalizeGit(runErr != nil || s.isError || s.isCancelled())
	fin := proto.RunFinish{SessionID: s.sessionID, CostUSD: s.cost, Result: s.result, Pushed: pushed, Head: head}
	switch {
	case s.isCancelled():
		fin.Status = "cancelled"
		fin.Error = "cancelled"
	case runErr != nil:
		fin.Status = "failed"
		fin.Error = runErr.Error()
		if tail := strings.TrimSpace(lastN(s.stderr.String(), 2000)); tail != "" {
			fin.Error += "\n" + tail
		}
	case s.isError:
		fin.Status = "failed"
		fin.Error = s.result
	default:
		fin.Status = "succeeded"
	}
	s.info("run finished: %s", fin.Status)
	return fin
}

func lastN(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (s *session) isCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

func userLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}},
	})
	return append(b, '\n')
}

// sendUser injects a message into the live Claude session, or queues it for a
// resumed session if the current process is already winding down.
func (s *session) sendUser(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit("human", map[string]string{"type": "human", "text": text})
	if s.stdin != nil {
		if _, err := s.stdin.Write(userLine(text)); err == nil {
			s.pending++
			return
		}
	}
	s.leftover = append(s.leftover, text)
}

func (s *session) cancel() {
	s.mu.Lock()
	s.cancelled = true
	p := s.proc
	s.mu.Unlock()
	if p != nil && p.Process != nil {
		syscall.Kill(-p.Process.Pid, syscall.SIGTERM)
		go func() {
			time.Sleep(8 * time.Second)
			syscall.Kill(-p.Process.Pid, syscall.SIGKILL)
		}()
	}
}

func (s *session) runClaude(messages []string) error {
	mcpCfg := map[string]any{"mcpServers": map[string]any{
		"multiclaude": map[string]any{
			"type":    "stdio",
			"command": s.r.self,
			"args":    []string{"mcp", "--server", s.r.cfg.Server, "--run", s.a.RunID},
			"env":     map[string]string{"MC_TOKEN": s.r.token},
		},
	}}
	mcpPath := filepath.Join(s.r.cfg.WorkDir, "mcp", s.a.RunID+".json")
	os.MkdirAll(filepath.Dir(mcpPath), 0o755)
	b, _ := json.Marshal(mcpCfg)
	os.WriteFile(mcpPath, b, 0o600)
	defer os.Remove(mcpPath)

	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", s.r.cfg.PermissionMode,
		"--mcp-config", mcpPath,
		"--append-system-prompt", s.a.SystemPrompt + s.environmentPrompt()}
	if s.sessionID != "" {
		args = append(args, "--resume", s.sessionID)
	}
	if s.a.Model != "" {
		args = append(args, "--model", s.a.Model)
	}
	args = append(args, s.r.cfg.ExtraArgs...)

	cmd := exec.Command(s.r.cfg.ClaudePath, args...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(),
		"MULTICLAUDE_RUN_ID="+s.a.RunID,
		"MULTICLAUDE_SERVER="+s.r.cfg.Server,
		"MULTICLAUDE_PROJECT="+s.a.ProjectSlug,
	)
	cmd.Env = append(cmd.Env, s.r.gitEnv()...) // git push/pull from the agent's shell is authenticated
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	s.stderr.Reset()
	cmd.Stderr = &limitedBuf{buf: &s.stderr, max: 64 << 10}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting claude: %w", err)
	}
	s.mu.Lock()
	s.proc = cmd
	s.stdin = stdin
	s.pending = 0
	for _, m := range messages {
		if _, err := stdin.Write(userLine(m)); err == nil {
			s.pending++
		}
	}
	s.mu.Unlock()

	closeStdin := func() {
		s.mu.Lock()
		if s.stdin != nil {
			s.stdin.Close() // no more input: claude finishes queued work and exits
			s.stdin = nil
		}
		s.mu.Unlock()
	}
	// Claude may fold a message injected mid-turn into the running turn, so
	// "one result per message" doesn't always hold. After a result with
	// messages still nominally pending, close stdin once output goes quiet.
	var idle *time.Timer
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if idle != nil {
			idle.Stop()
			idle = nil
		}
		var ev struct {
			Type      string  `json:"type"`
			Subtype   string  `json:"subtype"`
			SessionID string  `json:"session_id"`
			Result    string  `json:"result"`
			IsError   bool    `json:"is_error"`
			Cost      float64 `json:"total_cost_usd"`
		}
		if json.Unmarshal(line, &ev) != nil {
			s.emit("raw", map[string]string{"type": "raw", "text": string(line)})
			continue
		}
		if ev.SessionID != "" {
			s.sessionID = ev.SessionID
		}
		s.emit(ev.Type, json.RawMessage(append([]byte{}, line...)))
		if ev.Type == "result" {
			s.result, s.isError = ev.Result, ev.IsError
			s.cost += ev.Cost
			s.mu.Lock()
			s.pending--
			done := s.pending <= 0
			s.mu.Unlock()
			if done {
				closeStdin()
			} else {
				idle = time.AfterFunc(20*time.Second, closeStdin)
			}
		}
	}
	err = cmd.Wait()
	s.mu.Lock()
	if s.stdin != nil {
		s.stdin.Close()
		s.stdin = nil
	}
	s.proc = nil
	s.mu.Unlock()
	if err != nil && !s.isCancelled() {
		return fmt.Errorf("claude exited: %v", err)
	}
	return nil
}

type limitedBuf struct {
	mu  sync.Mutex
	buf *bytes.Buffer
	max int
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() < l.max {
		l.buf.Write(p)
	}
	return len(p), nil
}
