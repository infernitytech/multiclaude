package server

import (
	"strings"
	"testing"
)

// A run whose machine row is gone (removed, re-enrolled under a new id) used
// to stay "running" forever and hold its issue: the issue showed "waiting for
// its previous session to stop" and was never picked up again.
func TestReapReleasesRunsOfVanishedMachines(t *testing.T) {
	s, err := New(t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO projects(slug,name,description,default_branch,review_mode,instructions,created_at) VALUES('p','P','','main','agent','',?)`, now())
	var pid, iid int64
	s.db.QueryRow(`SELECT id FROM projects WHERE slug='p'`).Scan(&pid)
	mustExec(`INSERT INTO issues(project_id,number,title,body,status,priority,run_id,created_at,updated_at) VALUES(?,15,'t','','review',2,'r-ghost',?,?)`, pid, now(), now())
	s.db.QueryRow(`SELECT id FROM issues WHERE number=15`).Scan(&iid)
	s.insertRun("r-ghost", pid, &iid, "node-that-is-gone", "review", "", nil)

	p, _ := s.projectByID(pid)
	i, _ := s.issueByID(iid)
	i.RunID = "" // as after the issue was released by hand
	if w := s.waitReason(p, i, nil, nil, nil); !strings.Contains(w, "no longer exists") {
		t.Fatalf("wait reason should name the vanished machine: %q", w)
	}

	s.reapStale(now() - 90_000)
	run, _ := s.runByID("r-ghost")
	if run.Status != "lost" {
		t.Fatalf("run of a vanished machine must be released, status %q", run.Status)
	}
	i, _ = s.issueByID(iid)
	if i.Status != "review" || i.RunID != "" {
		t.Fatalf("review should be back in the queue: status %q run %q", i.Status, i.RunID)
	}
}
