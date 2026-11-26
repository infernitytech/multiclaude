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

// maybeSelfUpdate replaces this binary with the server's runner build when the
// versions differ and no session is running, then restarts in place. Runners
// thereby follow server upgrades without anyone touching the machines.
// Disable with MC_NO_SELF_UPDATE=1. Dev builds never update.
func (r *Runner) maybeSelfUpdate(serverVersion string) {
	if serverVersion == "" || serverVersion == proto.Version || serverVersion == "dev" || proto.Version == "dev" ||
		os.Getenv("MC_NO_SELF_UPDATE") != "" || r.updateTried[serverVersion] {
		return
	}
	r.mu.Lock()
	busy := len(r.sessions) > 0
	r.mu.Unlock()
	if busy {
		return // try again after the running sessions are done
	}
	r.updateTried[serverVersion] = true
	if err := r.selfUpdate(serverVersion); err != nil {
		log.Printf("self-update to %s failed: %v (continuing with %s)", serverVersion, err, proto.Version)
	}
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
