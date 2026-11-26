package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"multiclaude/internal/proto"
)

// A message folded into the running turn leaves one message "pending" after
// the result; output after the result (status/system events) used to cancel
// the quiet timer for good, so claude waited on stdin forever.
func TestSessionEndsWhenOutputContinuesAfterResult(t *testing.T) {
	old := idleClose
	idleClose = 300 * time.Millisecond
	defer func() { idleClose = old }()

	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	os.WriteFile(fake, []byte(`#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"s1"}'
echo '{"type":"result","result":"done","session_id":"s1"}'
sleep 0.1
echo '{"type":"system","subtype":"status"}'
cat >/dev/null   # waits until stdin is closed, like claude -p --input-format stream-json
`), 0o755)

	r := &Runner{cfg: Config{WorkDir: dir, ClaudePath: fake, PermissionMode: "default"}, self: "/bin/true"}
	s := newSession(r, &proto.Assignment{RunID: "t1"})
	s.dir = dir
	done := make(chan error, 1)
	go func() { done <- s.runClaude([]string{"task", "a message injected mid-turn"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		s.cancel()
		t.Fatal("session hung after its result (stdin never closed)")
	}
	if s.result != "done" {
		t.Fatalf("result %q", s.result)
	}
}
