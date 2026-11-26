package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"multiclaude/internal/proto"
)

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- lookups ---------------------------------------------------------------

func (s *Server) projectBySlug(slug string) (*Project, error) {
	return scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE slug=?`, slug))
}

func (s *Server) projectByID(id int64) (*Project, error) {
	return scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id=?`, id))
}

func (s *Server) issueByNumber(pid int64, n int) (*Issue, error) {
	return scanIssue(s.db.QueryRow(`SELECT `+issueCols+` FROM issues WHERE project_id=? AND number=?`, pid, n))
}

func (s *Server) issueByID(id int64) (*Issue, error) {
	return scanIssue(s.db.QueryRow(`SELECT `+issueCols+` FROM issues WHERE id=?`, id))
}

func (s *Server) node(id string) (*Node, error) {
	n, err := scanNode(s.db.QueryRow(`SELECT `+nodeCols+` FROM nodes WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	n.Projects = s.nodeProjects(id)
	n.Load = loadOf(id)
	n.UpdatingTo = drainingTo(id)
	return n, nil
}

func (s *Server) nodeByName(name string) (*Node, error) {
	var id string
	if err := s.db.QueryRow(`SELECT id FROM nodes WHERE name=? OR id=? ORDER BY last_seen DESC LIMIT 1`, name, name).Scan(&id); err != nil {
		return nil, err
	}
	return s.node(id)
}

func (s *Server) nodeProjects(nodeID string) []int64 {
	rows, err := s.db.Query(`SELECT project_id FROM node_projects WHERE node_id=?`, nodeID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (s *Server) runByID(id string) (*Run, error) {
	return scanRun(s.db.QueryRow(runSelect+`WHERE r.id=?`, id))
}

func (s *Server) runningCount(nodeID string) int {
	var c int
	s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE node_id=? AND status='running'`, nodeID).Scan(&c)
	return c
}

func (s *Server) addComment(issueID int64, author, body string) {
	s.db.Exec(`INSERT INTO comments(issue_id,author,body,created_at) VALUES(?,?,?,?)`, issueID, author, body, now())
	s.db.Exec(`UPDATE issues SET updated_at=? WHERE id=?`, now(), issueID)
}

func (s *Server) postMessage(pid int64, author, target, body string) Message {
	m := Message{ProjectID: pid, Author: author, Target: target, Body: body, CreatedAt: now()}
	res, err := s.db.Exec(`INSERT INTO messages(project_id,author,target,body,created_at) VALUES(?,?,?,?,?)`, pid, author, target, body, m.CreatedAt)
	if err == nil {
		m.ID, _ = res.LastInsertId()
	}
	s.emit(event{Type: "message", ProjectID: pid, Data: m})
	return m
}

func (s *Server) issueChanged(pid int64) { s.emit(event{Type: "issues", ProjectID: pid}) }

// ---- runner protocol -----------------------------------------------------------

func (s *Server) runnerRegister(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterRequest
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	nodeID, newTok, err := s.enroll(r, req.NodeID, req.JoinToken)
	if err != nil {
		s.logf("auth: refused runner registration from %s (%s): %v", clientIP(r), req.Name, err)
		unauthorized(w, err.Error())
		return
	}
	req.NodeID = nodeID
	if req.Name == "" {
		req.Name = req.Hardware.Hostname
	}
	if req.MaxConcurrency <= 0 {
		req.MaxConcurrency = 1
	}
	if len(req.Roles) == 0 {
		req.Roles = []string{"worker", "reviewer"}
	}
	t := now()
	_, err = s.db.Exec(`INSERT INTO nodes(id,name,hardware,caps,roles,max_conc,claude_path,claude_version,claude_auth,claude_account,runner_version,last_seen,registered_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, hardware=excluded.hardware, caps=excluded.caps, roles=excluded.roles,
		max_conc=excluded.max_conc, claude_path=excluded.claude_path, claude_version=excluded.claude_version, claude_auth=excluded.claude_auth,
		claude_account=excluded.claude_account, runner_version=excluded.runner_version, last_seen=excluded.last_seen`,
		req.NodeID, req.Name, jsonStr(req.Hardware), jsonStr(req.Capabilities), jsonStr(req.Roles), req.MaxConcurrency,
		req.ClaudePath, req.ClaudeVersion, req.ClaudeAuth, req.ClaudeAccount, req.RunnerVersion, t, t)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if newTok != "" {
		s.db.Exec(`UPDATE nodes SET token_hash=? WHERE id=?`, tokenHash(newTok), req.NodeID)
		s.logf("auth: enrolled machine %s (%s) from %s", req.Name, req.NodeID, clientIP(r))
	}
	// A (re)starting runner has no active runs: release whatever it held.
	rows, _ := s.db.Query(`SELECT id FROM runs WHERE node_id=? AND status='running'`, req.NodeID)
	var stale []string
	for rows != nil && rows.Next() {
		var id string
		rows.Scan(&id)
		stale = append(stale, id)
	}
	if rows != nil {
		rows.Close()
	}
	for _, id := range stale {
		s.finishRun(id, proto.RunFinish{Status: "lost", Error: "runner restarted"})
	}
	s.drain(req.NodeID)
	s.emit(event{Type: "nodes"})
	s.logf("node registered: %s (%s) caps=%v roles=%v", req.Name, req.NodeID, req.Capabilities, req.Roles)
	writeJSON(w, proto.RegisterResponse{NodeID: req.NodeID, NodeToken: newTok, ServerVersion: proto.Version})
}

func (s *Server) runnerPoll(w http.ResponseWriter, r *http.Request) {
	var req proto.PollRequest
	if err := readJSON(r, &req); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	req.NodeID = nodeFrom(r) // a machine can only poll for itself
	// A runner on another version that updates itself gets no new work: it
	// finishes its sessions, then restarts as the server's version.
	if req.Draining == "" && req.AutoUpdate && req.Version != "" && req.Version != proto.Version && proto.Version != "dev" {
		req.Draining = proto.Version
		setDraining(req.NodeID, req.Draining)
		s.touch(req.NodeID)
		writeJSON(w, proto.PollResponse{Commands: nzc(s.drain(req.NodeID)), ServerVersion: proto.Version})
		return
	}
	setDraining(req.NodeID, req.Draining)
	n, err := s.node(req.NodeID)
	if err != nil {
		httpErr(w, 404, "unknown node, re-register")
		return
	}
	wasOffline := !n.Online
	s.touch(n.ID)
	if wasOffline {
		s.emit(event{Type: "nodes"})
	}

	// Reconcile: runs the server thinks are running but the runner doesn't know.
	active := map[string]bool{}
	for _, id := range req.ActiveRuns {
		active[id] = true
	}
	rows, _ := s.db.Query(`SELECT id FROM runs WHERE node_id=? AND status='running' AND started_at < ?`, n.ID, now()-60_000)
	var orphan []string
	for rows != nil && rows.Next() {
		var id string
		rows.Scan(&id)
		if !active[id] {
			orphan = append(orphan, id)
		}
	}
	if rows != nil {
		rows.Close()
	}
	for _, id := range orphan {
		s.finishRun(id, proto.RunFinish{Status: "lost", Error: "runner no longer tracks this run"})
	}
	// The other direction: sessions the runner still runs although the server
	// has moved on (cancel lost in transit, issue moved, project stopped, run
	// already marked ended). Re-send the cancel until the runner reports it gone.
	for _, id := range req.ActiveRuns {
		if s.superseded(id) {
			s.enqueueOnce(n.ID, proto.Command{Type: "cancel", RunID: id})
		}
	}
	gen := s.pollStarted(n.ID)

	s.mu.Lock()
	ch := s.notify[n.ID]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.notify[n.ID] = ch
	}
	s.mu.Unlock()

	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		// A newer poll from this machine (it reconnected) or a vanished client:
		// stop here without taking commands or claiming work nobody receives.
		if r.Context().Err() != nil || !s.currentPoll(n.ID, gen) {
			return
		}
		cmds := s.drain(n.ID)
		if n, err = s.node(req.NodeID); err == nil {
			if free := n.MaxConc - s.runningCount(n.ID); free > 0 && !n.Paused && req.Draining == "" {
				cmds = append(cmds, s.claimWork(n, free)...)
			}
		}
		if len(cmds) > 0 {
			if r.Context().Err() != nil || !s.currentPoll(n.ID, gen) {
				s.requeue(n.ID, cmds) // the next poll delivers them
				return
			}
			s.touch(req.NodeID)
			writeJSON(w, proto.PollResponse{Commands: cmds, ServerVersion: proto.Version})
			return
		}
		s.mu.Lock()
		wc := s.workCh
		s.mu.Unlock()
		select {
		case <-ch:
		case <-wc:
		case <-deadline.C:
			s.touch(req.NodeID)
			writeJSON(w, proto.PollResponse{Commands: []proto.Command{}, ServerVersion: proto.Version})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) touch(nodeID string) {
	s.db.Exec(`UPDATE nodes SET last_seen=? WHERE id=?`, now(), nodeID)
}

func (s *Server) runnerEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var evs []proto.RunEvent
	if err := readJSON(r, &evs); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	run, err := s.runByID(id)
	if err != nil || run.NodeID != nodeFrom(r) {
		httpErr(w, 404, "unknown run")
		return
	}
	s.touch(run.NodeID)
	t := now()
	for _, e := range evs {
		data := string(e.Data)
		if len(data) > 200_000 {
			data = jsonStr(map[string]any{"type": e.Type, "truncated": true, "preview": data[:20_000]})
			e.Data = json.RawMessage(data)
		}
		res, err := s.db.Exec(`INSERT INTO run_events(run_id,ts,type,data) VALUES(?,?,?,?)`, id, t, e.Type, data)
		var eid int64
		if err == nil {
			eid, _ = res.LastInsertId()
		}
		s.emit(event{Type: "run_event", RunID: id, ProjectID: run.ProjectID,
			Data: map[string]any{"id": eid, "ts": t, "type": e.Type, "data": json.RawMessage(e.Data)}})
		s.trackActivity(run, e.Type, e.Data)

		var m struct {
			Type      string  `json:"type"`
			Subtype   string  `json:"subtype"`
			SessionID string  `json:"session_id"`
			Result    string  `json:"result"`
			Cost      float64 `json:"total_cost_usd"`
			Model     string  `json:"model"`
		}
		if json.Unmarshal(e.Data, &m) != nil {
			continue
		}
		if m.Type == "system" && m.Subtype == "init" && m.Model != "" && m.Model != run.Model {
			run.Model = m.Model
			s.db.Exec(`UPDATE runs SET model=? WHERE id=?`, m.Model, id)
			s.emit(event{Type: "runs", ProjectID: run.ProjectID})
		}
		if m.SessionID != "" && run.SessionID == "" {
			run.SessionID = m.SessionID
			s.db.Exec(`UPDATE runs SET session_id=? WHERE id=?`, m.SessionID, id)
		}
		if m.Type == "result" {
			s.db.Exec(`UPDATE runs SET cost_usd=cost_usd+? WHERE id=?`, m.Cost, id)
			if s.takeReply(id) && strings.TrimSpace(m.Result) != "" {
				s.postMessage(run.ProjectID, run.NodeName, "reply:"+id, m.Result)
			}
		}
	}
	w.WriteHeader(204)
}

// pending chat replies per run: each human message injected into a run
// expects one "result" which is mirrored back into the project chat.
// Stored in the database so replies survive a server restart.
func (s *Server) expectReply(runID string) {
	s.db.Exec(`UPDATE runs SET pending_replies=pending_replies+1 WHERE id=?`, runID)
}

func (s *Server) takeReply(runID string) bool {
	res, err := s.db.Exec(`UPDATE runs SET pending_replies=pending_replies-1 WHERE id=? AND pending_replies>0`, runID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Server) runnerFinish(w http.ResponseWriter, r *http.Request) {
	var fin proto.RunFinish
	if err := readJSON(r, &fin); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if run, err := s.runByID(r.PathValue("id")); err != nil || run.NodeID != nodeFrom(r) {
		httpErr(w, 404, "unknown run")
		return
	}
	s.finishRun(r.PathValue("id"), fin)
	w.WriteHeader(204)
}

// ---- scheduling -------------------------------------------------------------------

func subset(need, have []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[strings.ToLower(h)] = true
	}
	for _, n := range need {
		if !set[strings.ToLower(n)] {
			return false
		}
	}
	return true
}

type candidate struct {
	issue   *Issue
	project *Project
	review  bool
}

// claimWork atomically assigns up to n claimable issues to the node.
func (s *Server) claimWork(n *Node, slots int) []proto.Command {
	if len(n.Projects) == 0 || (!n.HasRole("worker") && !n.HasRole("reviewer")) {
		return nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(n.Projects)), ",")
	args := []any{}
	for _, p := range n.Projects {
		args = append(args, p)
	}
	// An issue whose previous session is still running (e.g. it was just moved
	// back to To do and that session is shutting down) waits until it has ended:
	// one issue never has two live sessions.
	rows, err := s.db.Query(`SELECT `+issueCols+` FROM issues WHERE run_id='' AND status IN ('todo','review') AND project_id IN (`+ph+`)
		AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.issue_id=issues.id AND r.status='running')`, args...)
	if err != nil {
		return nil
	}
	var issues []*Issue
	for rows.Next() {
		if i, err := scanIssue(rows); err == nil {
			issues = append(issues, i)
		}
	}
	rows.Close()

	projects := map[int64]*Project{}
	var cands []candidate
	for _, i := range issues {
		p := projects[i.ProjectID]
		if p == nil {
			if p, err = s.projectByID(i.ProjectID); err != nil {
				continue
			}
			projects[i.ProjectID] = p
		}
		if !p.Dispatch || !canEverRun(n, i.Requires) {
			continue
		}
		switch i.Status {
		case "todo":
			if n.HasRole("worker") && s.depsDone(i) {
				cands = append(cands, candidate{i, p, false})
			}
		case "review":
			if n.HasRole("reviewer") && p.ReviewMode == "agent" {
				cands = append(cands, candidate{i, p, true})
			}
		}
	}
	// Reviews first (they unblock merges), then work that needs this machine's
	// special capabilities, then priority / board order.
	sort.SliceStable(cands, func(a, b int) bool {
		x, y := cands[a], cands[b]
		if x.review != y.review {
			return x.review
		}
		xs, ys := len(x.issue.Requires) > 0, len(y.issue.Requires) > 0
		if xs != ys {
			return xs
		}
		if x.issue.Priority != y.issue.Priority {
			return x.issue.Priority < y.issue.Priority
		}
		if x.issue.Sort != y.issue.Sort {
			return x.issue.Sort < y.issue.Sort
		}
		return x.issue.Number < y.issue.Number
	})

	// Fair share between projects: each free slot goes to the project with the
	// fewest sessions on this machine right now (reviews still first), so a
	// machine serving several projects doesn't drain one queue before the next.
	load := map[int64]int{}
	if rows, err := s.db.Query(`SELECT project_id, COUNT(*) FROM runs WHERE node_id=? AND status='running' AND kind<>'chat' GROUP BY project_id`, n.ID); err == nil {
		for rows.Next() {
			var pid int64
			var k int
			if rows.Scan(&pid, &k) == nil {
				load[pid] = k
			}
		}
		rows.Close()
	}
	pick := func() candidate {
		best := 0
		for k := 1; k < len(cands); k++ {
			x, b := cands[k], cands[best]
			if x.review != b.review {
				if x.review {
					best = k
				}
				continue
			}
			if load[x.project.ID] < load[b.project.ID] {
				best = k
			}
		}
		c := cands[best]
		cands = append(cands[:best], cands[best+1:]...)
		return c
	}

	var cmds []proto.Command
	leased := s.gpuLeases(n.ID)
	for len(cands) > 0 {
		if len(cmds) >= slots {
			break
		}
		c := pick()
		need, _ := parseGPUNeed(c.issue.Requires)
		gpus, ok := allocGPUs(n, need, leased)
		if !ok {
			continue // not enough free GPUs right now; it gets them when a GPU run ends
		}
		runID := newID()
		kind, newStatus := "work", "in_progress"
		if c.review {
			kind, newStatus = "review", "review"
		}
		branch := fmt.Sprintf("issue-%d", c.issue.Number)
		res, err := s.db.Exec(`UPDATE issues SET run_id=?, assignee=?, status=?, branch=?, updated_at=? WHERE id=? AND run_id='' AND status=?`,
			runID, n.Name, newStatus, branch, now(), c.issue.ID, c.issue.Status)
		if err != nil {
			continue
		}
		if k, _ := res.RowsAffected(); k != 1 {
			continue
		}
		c.issue.Branch = branch
		a := s.buildAssignment(runID, kind, c.project, c.issue, n, "")
		s.gpuSetup(a, n, gpus, need.count > 0)
		s.insertRun(runID, c.project.ID, &c.issue.ID, n.ID, kind, a.Prompt, gpus)
		for _, g := range gpus {
			leased[g] = runID
		}
		cmds = append(cmds, proto.Command{Type: "assign", Assignment: a})
		load[c.project.ID]++
		verb := "started working on"
		if c.review {
			verb = "started reviewing"
		}
		s.addComment(c.issue.ID, n.Name, fmt.Sprintf("🤖 %s this issue (run `%s`).", verb, runID))
		s.issueChanged(c.project.ID)
	}
	if len(cmds) > 0 {
		s.emit(event{Type: "nodes"})
	}
	return cmds
}

func (s *Server) depsDone(i *Issue) bool {
	for _, d := range i.DependsOn {
		var st string
		if err := s.db.QueryRow(`SELECT status FROM issues WHERE project_id=? AND number=?`, i.ProjectID, d).Scan(&st); err == nil && st != "done" {
			return false
		}
	}
	return true
}

func (s *Server) insertRun(id string, pid int64, issueID *int64, nodeID, kind, prompt string, gpus []int) {
	s.db.Exec(`INSERT INTO runs(id,project_id,issue_id,node_id,kind,status,prompt,started_at,gpus) VALUES(?,?,?,?,?,'running',?,?,?)`,
		id, pid, issueID, nodeID, kind, prompt, now(), jsonStr(append([]int{}, gpus...)))
	s.emit(event{Type: "runs", ProjectID: pid})
}

// startChatRun starts a conversational run on a node, resuming its previous
// chat session for that project so the conversation has memory.
func (s *Server) startChatRun(p *Project, n *Node, author, text string) string {
	runID := newID()
	a := s.buildAssignment(runID, "chat", p, nil, n, chatPrompt(author, text))
	s.gpuSetup(a, n, nil, false)
	s.db.QueryRow(`SELECT session_id FROM chat_sessions WHERE node_id=? AND project_id=?`, n.ID, p.ID).Scan(&a.ResumeSession)
	s.insertRun(runID, p.ID, nil, n.ID, "chat", a.Prompt, nil)
	s.expectReply(runID)
	s.enqueue(n.ID, proto.Command{Type: "assign", Assignment: a})
	s.emit(event{Type: "nodes"})
	return runID
}

// deliverChat routes a chat message to agents. Running agents get it injected
// live; idle assigned machines start a chat session.
func (s *Server) deliverChat(p *Project, targets []*Node, author, body string) string {
	var notes []string
	for _, n := range targets {
		if !n.Online || n.Paused {
			continue
		}
		rows, err := s.db.Query(`SELECT id FROM runs WHERE node_id=? AND project_id=? AND status='running'`, n.ID, p.ID)
		if err != nil {
			continue
		}
		var runs []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			runs = append(runs, id)
		}
		rows.Close()
		msg := fmt.Sprintf("[Project chat message from %s]\n%s", author, body)
		if len(runs) > 0 {
			for _, id := range runs {
				s.expectReply(id)
				s.enqueue(n.ID, proto.Command{Type: "message", RunID: id, Message: msg})
			}
			notes = append(notes, fmt.Sprintf("%s: injected into %d running session(s)", n.Name, len(runs)))
		} else if s.runningCount(n.ID) < n.MaxConc && drainingTo(n.ID) == "" {
			s.startChatRun(p, n, author, body)
			notes = append(notes, n.Name+": started a chat session")
		} else {
			notes = append(notes, n.Name+": busy with other projects (message is readable via read_chat)")
		}
	}
	if len(notes) == 0 {
		return "No online machines received this message live. Agents will see it via read_chat."
	}
	return strings.Join(notes, "; ")
}

// ---- finishing runs ------------------------------------------------------------------

func (s *Server) finishRun(id string, fin proto.RunFinish) {
	defer forgetAgentMsgBudget(id)
	defer forgetActivity(id)
	run, err := s.runByID(id)
	if err != nil || run.Status != "running" {
		return
	}
	if fin.Status == "" {
		fin.Status = "failed"
	}
	s.db.Exec(`UPDATE runs SET status=?, ended_at=?, session_id=CASE WHEN ?<>'' THEN ? ELSE session_id END, result=?, error=? WHERE id=?`,
		fin.Status, now(), fin.SessionID, fin.SessionID, fin.Result, fin.Error, id)
	defer func() {
		s.emit(event{Type: "runs", ProjectID: run.ProjectID})
		s.emit(event{Type: "nodes"})
		s.issueChanged(run.ProjectID)
		s.workAvailable()
	}()
	// Re-read flags set by tool calls during the run.
	run, _ = s.runByID(id)
	p, err := s.projectByID(run.ProjectID)
	if err != nil {
		return
	}

	if run.Kind == "chat" {
		if run.SessionID != "" && fin.Status == "succeeded" {
			s.db.Exec(`INSERT INTO chat_sessions(node_id,project_id,session_id) VALUES(?,?,?)
				ON CONFLICT(node_id,project_id) DO UPDATE SET session_id=excluded.session_id`, run.NodeID, run.ProjectID, run.SessionID)
		}
		if fin.Status != "succeeded" && fin.Status != "cancelled" {
			s.postMessage(p.ID, run.NodeName, "reply:"+id, "⚠️ chat session failed: "+firstNonEmpty(fin.Error, fin.Result))
		}
		return
	}
	if run.IssueID == nil {
		return
	}
	issue, err := s.issueByID(*run.IssueID)
	if err != nil || issue.RunID != id {
		return // human moved the issue away meanwhile
	}
	release := func(status string, attemptInc int, comment string) {
		s.db.Exec(`UPDATE issues SET status=?, run_id='', attempts=attempts+?, updated_at=? WHERE id=?`, status, attemptInc, now(), issue.ID)
		if comment != "" {
			s.addComment(issue.ID, "multiclaude", comment)
		}
	}
	if fin.Status == "lost" || fin.Status == "cancelled" {
		back := "todo"
		if run.Kind == "review" {
			back = "review"
		}
		release(back, 0, fmt.Sprintf("Run on %s ended (%s): %s — returned to the queue.", run.NodeName, fin.Status, fin.Error))
		return
	}
	failNote := func(what string) string {
		tail := firstNonEmpty(fin.Error, fin.Result)
		if len(tail) > 1500 {
			tail = "…" + tail[len(tail)-1500:]
		}
		return fmt.Sprintf("Run on %s %s (status: %s).\n\n%s", run.NodeName, what, fin.Status, tail)
	}
	retryStatus := func() string {
		if issue.Attempts+1 >= 3 {
			return "blocked"
		}
		return issue.Status
	}

	switch run.Kind {
	case "work":
		if !run.Completed {
			st := "todo"
			if issue.Attempts+1 >= 3 {
				st = "blocked"
			}
			release(st, 1, failNote("ended without calling complete_work"))
			return
		}
		switch p.ReviewMode {
		case "none":
			s.mergeIssue(p, issue, run.NodeName)
		default:
			release("review", 0, "")
		}
	case "review":
		switch run.Verdict {
		case "approve":
			s.mergeIssue(p, issue, run.NodeName)
		case "request_changes":
			release("todo", 0, "")
		default:
			st := retryStatus()
			release(st, 1, failNote("ended without a review verdict"))
		}
	}
}

// mergeIssue merges the issue branch into the default branch on the server.
func (s *Server) mergeIssue(p *Project, issue *Issue, by string) error {
	branch := issue.Branch
	if branch == "" {
		branch = fmt.Sprintf("issue-%d", issue.Number)
	}
	repos := s.projectRepos(p)
	multi := len(repos) > 1
	fail := func(err error, rp *Repo) error {
		where := ""
		if multi {
			where = " in repository `" + rp.Name + "`"
		}
		msg := fmt.Sprintf("❌ Merge of `%s` into `%s`%s failed: %v\n\nPlease merge `origin/%s` into the branch%s, resolve conflicts and push again. Nothing was merged in any repository.", branch, rp.DefaultBranch, where, err, rp.DefaultBranch, where)
		if !errors.Is(err, errConflict) {
			msg = fmt.Sprintf("❌ Merge of `%s`%s failed: %v", branch, where, err)
		}
		// Counts as a failed attempt, so an unresolvable conflict can't loop forever.
		s.db.Exec(`UPDATE issues SET status=CASE WHEN attempts+1>=3 THEN 'blocked' ELSE 'todo' END, attempts=attempts+1, run_id='', updated_at=? WHERE id=?`, now(), issue.ID)
		s.addComment(issue.ID, "multiclaude", msg)
		return err
	}
	// Check every repo first, so a conflict in one never leaves the others half-merged.
	var plans []*mergePlan
	var names []string
	for _, rp := range repos {
		pl, err := s.planMerge(rp.Key, rp.DefaultBranch, branch, fmt.Sprintf("Merge %s: %s (#%d)", branch, issue.Title, issue.Number))
		if errors.Is(err, errNoBranch) {
			continue // not touched in this repo
		}
		if err != nil {
			return fail(err, rp)
		}
		plans = append(plans, pl)
		if !pl.noop {
			names = append(names, fmt.Sprintf("`%s` (%s)", rp.Name, short(pl.newSHA)))
		}
	}
	if len(plans) == 0 {
		return fail(fmt.Errorf("branch %s does not exist on the server (was it pushed?)", branch), repos[0])
	}
	for _, pl := range plans {
		if err := s.applyMerge(pl); err != nil {
			return fail(err, repos[0])
		}
	}
	s.db.Exec(`UPDATE issues SET status='done', run_id='', updated_at=? WHERE id=?`, now(), issue.ID)
	what := fmt.Sprintf("into `%s` (%s)", p.DefaultBranch, short(plans[0].newSHA))
	if multi {
		what = "in " + strings.Join(names, ", ")
		if len(names) == 0 {
			what = "(no new commits)"
		}
	}
	s.addComment(issue.ID, "multiclaude", fmt.Sprintf("✅ Merged `%s` %s — approved by %s.", branch, what, by))
	s.workAvailable() // dependants may be unblocked
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func firstNonEmpty(a ...string) string {
	for _, x := range a {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}

// cancelIssueRun stops the run currently attached to an issue (if any).
func (s *Server) cancelIssueRun(issue *Issue) {
	if issue.RunID == "" {
		return
	}
	run, err := s.runByID(issue.RunID)
	if err == nil && run.Status == "running" {
		s.enqueue(run.NodeID, proto.Command{Type: "cancel", RunID: run.ID})
	}
}

var _ = sql.ErrNoRows

func nzc(c []proto.Command) []proto.Command {
	if c == nil {
		return []proto.Command{}
	}
	return c
}
