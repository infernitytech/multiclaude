package runner

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"multiclaude/internal/proto"
)

// maybeSelfUpdate follows server upgrades: when the server runs another
// version, the runner stops taking new work ("draining"), lets the running
// sessions finish, then replaces this binary with the server's build and
// restarts in place. Disable with MC_NO_SELF_UPDATE=1. Dev builds never update.
func (r *Runner) maybeSelfUpdate(serverVersion string) {
	if serverVersion == "" || serverVersion == proto.Version || serverVersion == "dev" || proto.Version == "dev" ||
		os.Getenv("MC_NO_SELF_UPDATE") != "" {
		r.pendingUpdate = ""
		return
	}
	if t, ok := r.updateTried[serverVersion]; ok && time.Since(t) < 15*time.Minute {
		return // last attempt failed; work normally and retry later
	}
	r.mu.Lock()
	busy := len(r.sessions) + len(r.finishing)
	r.mu.Unlock()
	if busy > 0 {
		if r.pendingUpdate != serverVersion {
			log.Printf("server runs %s (this runner %s): taking no new work, updating once the %d running session(s) are done", serverVersion, proto.Version, busy)
		}
		r.pendingUpdate = serverVersion
		return
	}
	r.updateTried[serverVersion] = time.Now()
	r.pendingUpdate = ""
	if err := r.selfUpdate(serverVersion); err != nil {
		log.Printf("self-update to %s failed: %v (continuing with %s, retrying in 15 min)", serverVersion, err, proto.Version)
	}
}

// canSelfUpdate: this runner will update itself to the server's version (so
// the server should send it no new work until it has). False after a failed
// attempt, so a runner that cannot update keeps working.
func (r *Runner) canSelfUpdate() bool {
	if proto.Version == "dev" || os.Getenv("MC_NO_SELF_UPDATE") != "" {
		return false
	}
	for _, t := range r.updateTried {
		if time.Since(t) < 15*time.Minute {
			return false
		}
	}
	return true
}

func (r *Runner) selfUpdate(want string) error {
	url := fmt.Sprintf("%s/download/multiclaude-runner-%s-%s", r.cfg.Server, runtime.GOOS, runtime.GOARCH)
	log.Printf("server runs %s, this runner %s: updating from %s", want, proto.Version, url)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	tmp := r.self + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// Sanity check: the new binary runs and reports the expected version.
	out, err := exec.Command(tmp, "version").Output()
	if got := strings.TrimSpace(string(out)); err != nil || got != want {
		os.Remove(tmp)
		return fmt.Errorf("downloaded binary reports version %q, want %q (%v)", got, want, err)
	}
	if err := os.Rename(tmp, r.self); err != nil {
		os.Remove(tmp)
		return err
	}
	log.Printf("updated to %s, restarting", want)
	return syscall.Exec(r.self, os.Args, os.Environ())
}
