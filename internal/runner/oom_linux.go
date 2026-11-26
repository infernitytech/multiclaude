package runner

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Memory pressure: a test suite or model load in one session must not take
// the runner (and with it every other session on the machine) down.
//
//  1. Agent processes are made the kernel's preferred OOM victims. Raising
//     oom_score_adj needs no privileges and is inherited by everything the
//     agent starts (builds, tests), so the runner itself survives.
//  2. systemd's default OOMPolicy=stop stops the whole unit when the kernel
//     kills any process in it. OOMPolicy=continue keeps the runner and the
//     other sessions alive; only the session whose process died is affected.

const agentOOMScore = 500

func protectFromOOM(pid int) {
	os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte(fmt.Sprint(agentOOMScore)), 0)
}

const runnerUnit = "multiclaude-runner.service"

// ensureOOMPolicy installs a drop-in with OOMPolicy=continue for the runner's
// systemd user unit, so machines installed before this setting existed pick
// it up without reinstalling. It only acts when running under that unit.
func ensureOOMPolicy() {
	if os.Getenv("INVOCATION_ID") == "" { // not started by systemd
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if _, err := os.Stat(filepath.Join(unitDir, runnerUnit)); err != nil {
		return // not our user unit (system service, container, …): leave it alone
	}
	dropIn := filepath.Join(unitDir, runnerUnit+".d", "oom.conf")
	want := "# Written by multiclaude-runner: an out-of-memory kill of one agent's process\n" +
		"# must not stop the runner and all other sessions.\n[Service]\nOOMPolicy=continue\n"
	if b, err := os.ReadFile(dropIn); err == nil && string(b) == want {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(dropIn, []byte(want), 0o644); err != nil {
		return
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		log.Printf("wrote %s, but systemctl --user daemon-reload failed: %v %s", dropIn, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("set OOMPolicy=continue for %s (%s)", runnerUnit, dropIn)
}
