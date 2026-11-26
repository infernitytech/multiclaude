package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRoot(t *testing.T) {
	wd := t.TempDir()
	r := &Runner{cfg: Config{WorkDir: wd}}
	def := filepath.Join(wd, "workspaces")

	if got, p := r.workspaceRoot(""); got != def || p != "" {
		t.Fatalf("empty: got %q %q", got, p)
	}
	custom := filepath.Join(t.TempDir(), "ws")
	if got, p := r.workspaceRoot(custom); got != custom || p != "" {
		t.Fatalf("absolute: got %q %q", got, p)
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("not created: %v", err)
	}
	t.Setenv("MC_TEST_WS", custom)
	if got, _ := r.workspaceRoot("$MC_TEST_WS/x"); got != filepath.Join(custom, "x") {
		t.Fatalf("env: got %q", got)
	}
	home, _ := os.UserHomeDir()
	if got, _ := r.workspaceRoot("~/.mc-ws-test-unused"); !strings.HasPrefix(got, home) {
		t.Fatalf("tilde: got %q", got)
	}
	os.RemoveAll(filepath.Join(home, ".mc-ws-test-unused"))
	if got, p := r.workspaceRoot("relative/dir"); got != def || p == "" {
		t.Fatalf("relative must fall back with a reason: %q %q", got, p)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	if got, p := r.workspaceRoot(filepath.Join(blocker, "sub")); got != def || p == "" {
		t.Fatalf("unusable must fall back with a reason: %q %q", got, p)
	}
}
