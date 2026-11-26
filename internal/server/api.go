package server

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"multiclaude/internal/proto"
)

func (s *Server) projectFromReq(w http.ResponseWriter, r *http.Request) *Project {
	p, err := s.projectBySlug(r.PathValue("slug"))
	if err != nil {
		httpErr(w, 404, "project not found")
		return nil
	}
	return p
}

func (s *Server) issueFromReq(w http.ResponseWriter, r *http.Request) (*Project, *Issue) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return nil, nil
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	i, err := s.issueByNumber(p.ID, n)
	if err != nil {
		httpErr(w, 404, "issue not found")
		return nil, nil
	}
	return p, i
}

func (s *Server) projectCounts(p *Project) {
	p.Counts = map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM issues WHERE project_id=? GROUP BY status`, p.ID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var c int
		rows.Scan(&st, &c)
		p.Counts[st] = c
	}
}

func (s *Server) allProjects() []*Project {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY name`)
	if err != nil {
		return nil
	}
	var ps []*Project
	for rows.Next() {
		if p, err := scanProject(rows); err == nil {
			ps = append(ps, p)
		}
	}
	rows.Close()
	for _, p := range ps {
		s.projectCounts(p)
	}
	return ps
}

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	nodes, _ := s.listNodes()
	var cost float64
	s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM runs`).Scan(&cost)
	writeJSON(w, map[string]any{"version": proto.Version, "latest_version": latestRelease(), "projects": nz2(s.allProjects()), "nodes": nzn(nodes), "total_cost_usd": cost})
}

func nz2(p []*Project) []*Project {
	if p == nil {
		return []*Project{}
	}
	return p
}
func nzn(n []*Node) []*Node {
	if n == nil {
		return []*Node{}
	}
	return n
}

func (s *Server) apiProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, nz2(s.allProjects()))
}

var nonSlug = regexp.MustCompile(`[^a-z0-9._-]+`)

func slugify(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-.")
}

func (s *Server) apiCreateProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string   `json:"name"`
		Slug          string   `json:"slug"`
		Description   string   `json:"description"`
		ImportURL     string   `json:"import_url"`
		ImportToken   string   `json:"import_token"`
		ReviewMode    string   `json:"review_mode"`
		DefaultBranch string   `json:"default_branch"`
		Instructions  string   `json:"instructions"`
		Nodes         []string `json:"nodes"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		httpErr(w, 400, "name is required")
		return
	}
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	}
	if !slugRe.MatchString(in.Slug) {
		httpErr(w, 400, "invalid slug (use a-z, 0-9, . _ -)")
		return
	}
	if in.DefaultBranch == "" && strings.TrimSpace(in.ImportURL) == "" {
		in.DefaultBranch = "main"
	}
	if in.ReviewMode != "human" && in.ReviewMode != "none" {
		in.ReviewMode = "agent"
	}
	if _, err := s.projectBySlug(in.Slug); err == nil {
		httpErr(w, 409, "a project with this slug already exists")
		return
	}
	if err := s.createRepoAuth(in.Slug, in.Name, in.Description, in.DefaultBranch, strings.TrimSpace(in.ImportURL), s.tokenFor(normalizeImport(in.ImportURL), in.ImportToken)); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	in.DefaultBranch = s.headBranch(in.Slug, in.DefaultBranch)
	res, err := s.db.Exec(`INSERT INTO projects(slug,name,description,default_branch,review_mode,instructions,created_at) VALUES(?,?,?,?,?,?,?)`,
		in.Slug, in.Name, in.Description, in.DefaultBranch, in.ReviewMode, in.Instructions, now())
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.ensurePrimaryRepos()
	for _, n := range in.Nodes {
		s.db.Exec(`INSERT OR IGNORE INTO node_projects(node_id,project_id) VALUES(?,?)`, n, id)
	}
	p, _ := s.projectByID(id)
	s.emit(event{Type: "projects"})
	s.emit(event{Type: "nodes"})
	s.workAvailable()
	writeJSON(w, p)
}

func (s *Server) apiProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	s.projectCounts(p)
	var nodes []string
	rows, _ := s.db.Query(`SELECT node_id FROM node_projects WHERE project_id=?`, p.ID)
	for rows != nil && rows.Next() {
		var id string
		rows.Scan(&id)
		nodes = append(nodes, id)
	}
	if rows != nil {
		rows.Close()
	}
	if nodes == nil {
		nodes = []string{}
	}
	writeJSON(w, map[string]any{"project": p, "nodes": nodes, "clone_url": baseURL(r) + "/git/" + p.Slug + ".git"})
}

func (s *Server) apiUpdateProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	var in struct {
		Name         *string `json:"name"`
		Description  *string `json:"description"`
		ReviewMode   *string `json:"review_mode"`
		Instructions *string `json:"instructions"`
		Dispatch     *bool   `json:"dispatch"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		p.Description = *in.Description
	}
	if in.ReviewMode != nil && (*in.ReviewMode == "agent" || *in.ReviewMode == "human" || *in.ReviewMode == "none") {
		p.ReviewMode = *in.ReviewMode
	}
	if in.Instructions != nil {
		p.Instructions = *in.Instructions
	}
	if in.Dispatch != nil {
		p.Dispatch = *in.Dispatch
	}
	s.db.Exec(`UPDATE projects SET name=?,description=?,review_mode=?,instructions=?,dispatch=? WHERE id=?`,
		p.Name, p.Description, p.ReviewMode, p.Instructions, p.Dispatch, p.ID)
	s.emit(event{Type: "projects"})
	s.workAvailable()
	writeJSON(w, p)
}

func (s *Server) apiDeleteProject(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	if r.URL.Query().Get("confirm") != p.Slug {
		httpErr(w, 400, "pass ?confirm=<slug> to delete")
		return
	}
	rows, _ := s.db.Query(`SELECT id, node_id FROM runs WHERE project_id=? AND status='running'`, p.ID)
	for rows != nil && rows.Next() {
		var id, node string
		rows.Scan(&id, &node)
		s.enqueue(node, proto.Command{Type: "cancel", RunID: id})
	}
	if rows != nil {
		rows.Close()
	}
	for _, q := range []string{
		`DELETE FROM comments WHERE issue_id IN (SELECT id FROM issues WHERE project_id=?)`,
		`DELETE FROM run_events WHERE run_id IN (SELECT id FROM runs WHERE project_id=?)`,
		`DELETE FROM runs WHERE project_id=?`, `DELETE FROM issues WHERE project_id=?`,
		`DELETE FROM messages WHERE project_id=?`, `DELETE FROM node_projects WHERE project_id=?`,
		`DELETE FROM chat_sessions WHERE project_id=?`,
		`DELETE FROM mirrors WHERE repo_id IN (SELECT id FROM repos WHERE project_id=?)`, `DELETE FROM repos WHERE project_id=?`,
		`DELETE FROM projects WHERE id=?`,
	} {
		s.db.Exec(q, p.ID)
	}
	// Keep the data around in a trash folder instead of deleting it.
	trash := filepath.Join(s.DataDir, "trash", fmt.Sprintf("%s-%d", p.Slug, time.Now().Unix()))
	os.MkdirAll(trash, 0o755)
	os.Rename(s.repoPath(p.Slug), filepath.Join(trash, "repo.git"))
	os.Rename(filepath.Join(s.DataDir, "lfs", p.Slug), filepath.Join(trash, "lfs"))
	os.Rename(filepath.Join(s.DataDir, "repos", p.Slug), filepath.Join(trash, "repos")) // further repos
	if more, _ := filepath.Glob(filepath.Join(s.DataDir, "lfs", p.Slug+"~*")); len(more) > 0 {
		for _, d := range more {
			os.Rename(d, filepath.Join(trash, "lfs-"+filepath.Base(d)))
		}
	}
	s.emit(event{Type: "projects"})
	w.WriteHeader(204)
}

// ---- issues -----------------------------------------------------------------

func (s *Server) apiIssues(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	if r.URL.Query().Has("limit") { // paginated: {items, total}
		q := queryFromReq(r)
		if q.Limit <= 0 || q.Limit > 500 {
			q.Limit = 100
		}
		items, total, err := s.queryIssues(p.ID, q)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"items": items, "total": total})
		return
	}
	issues, err := s.listIssues(p.ID, r.URL.Query().Get("status"), r.URL.Query().Get("q"))
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, issues)
}

func (s *Server) apiCreateIssue(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	var in issueInput
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	i, err := s.createIssue(p, in, "human")
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, i)
}

func (s *Server) apiIssue(w http.ResponseWriter, r *http.Request) {
	_, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	s.loadIssueDetail(i)
	writeJSON(w, i)
}

func (s *Server) apiUpdateIssue(w http.ResponseWriter, r *http.Request) {
	p, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	var in issueInput
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	i, err := s.updateIssue(p, i, in, "human")
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	writeJSON(w, i)
}

func (s *Server) apiDeleteIssue(w http.ResponseWriter, r *http.Request) {
	p, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	s.cancelIssueRun(i)
	s.db.Exec(`DELETE FROM comments WHERE issue_id=?`, i.ID)
	s.db.Exec(`DELETE FROM issues WHERE id=?`, i.ID)
	s.issueChanged(p.ID)
	w.WriteHeader(204)
}

func (s *Server) apiComment(w http.ResponseWriter, r *http.Request) {
	p, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Body) == "" {
		httpErr(w, 400, "body required")
		return
	}
	s.addComment(i.ID, "human", in.Body)
	// If an agent is working on it right now, tell it.
	if i.RunID != "" {
		if run, err := s.runByID(i.RunID); err == nil && run.Status == "running" {
			s.injectIntoRun(run, fmt.Sprintf("[Human comment on issue #%d]\n%s", i.Number, in.Body))
		}
	}
	s.issueChanged(p.ID)
	w.WriteHeader(204)
}

func (s *Server) apiMerge(w http.ResponseWriter, r *http.Request) {
	p, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	s.cancelIssueRun(i)
	if err := s.mergeIssue(p, i, "human"); err != nil {
		s.issueChanged(p.ID)
		httpErr(w, 409, err.Error())
		return
	}
	s.issueChanged(p.ID)
	w.WriteHeader(204)
}

func (s *Server) apiRequestChanges(w http.ResponseWriter, r *http.Request) {
	p, i := s.issueFromReq(w, r)
	if i == nil {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	readJSON(r, &in)
	s.cancelIssueRun(i)
	s.db.Exec(`UPDATE issues SET status='todo', run_id='', attempts=0, updated_at=? WHERE id=?`, now(), i.ID)
	s.addComment(i.ID, "human", "🔁 Changes requested:\n\n"+in.Body)
	s.issueChanged(p.ID)
	s.workAvailable()
	w.WriteHeader(204)
}

// ---- chat ----------------------------------------------------------------------

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	msgs, err := s.messages(p.ID, 300)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, msgs)
}

func (s *Server) apiPostMessage(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	var in struct {
		Body   string `json:"body"`
		Target string `json:"target"` // "" = all assigned machines, or a node id
		Silent bool   `json:"silent"` // only post, don't wake agents
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Body) == "" {
		httpErr(w, 400, "body required")
		return
	}
	m := s.postMessage(p.ID, "human", in.Target, in.Body)
	if in.Silent {
		writeJSON(w, map[string]any{"message": m, "delivery": "posted only"})
		return
	}
	var targets []*Node
	if in.Target != "" {
		n, err := s.node(in.Target)
		if err != nil {
			httpErr(w, 404, "unknown machine")
			return
		}
		targets = []*Node{n}
	} else {
		nodes, _ := s.listNodes()
		for _, n := range nodes {
			for _, pid := range n.Projects {
				if pid == p.ID {
					targets = append(targets, n)
				}
			}
		}
	}
	note := s.deliverChat(p, targets, "human", in.Body)
	s.postMessage(p.ID, "system", "", "↳ "+note)
	writeJSON(w, map[string]any{"message": m, "delivery": note})
}

// ---- runs ----------------------------------------------------------------------

func (s *Server) apiProjectRuns(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	rows, err := s.db.Query(runSelect+`WHERE r.project_id=? ORDER BY r.started_at DESC LIMIT 200`, p.ID)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		if x, err := scanRun(rows); err == nil {
			x.Prompt = ""
			out = append(out, x)
		}
	}
	writeJSON(w, out)
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.runByID(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "run not found")
		return
	}
	if run.Status == "running" {
		run.Activity = activityOf(run.ID)
	}
	writeJSON(w, run)
}

func (s *Server) apiRunEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	rows, err := s.db.Query(`SELECT id,ts,type,data FROM run_events WHERE run_id=? AND id>? ORDER BY id LIMIT 5000`, r.PathValue("id"), after)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type ev struct {
		ID   int64  `json:"id"`
		TS   int64  `json:"ts"`
		Type string `json:"type"`
		Data rawStr `json:"data"`
	}
	out := []ev{}
	for rows.Next() {
		var e ev
		var d string
		rows.Scan(&e.ID, &e.TS, &e.Type, &d)
		e.Data = rawStr(d)
		out = append(out, e)
	}
	writeJSON(w, out)
}

// rawStr marshals as raw JSON when valid, otherwise as a string.
type rawStr string

func (r rawStr) MarshalJSON() ([]byte, error) {
	b := []byte(r)
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		return b, nil
	}
	return []byte(strconv.Quote(string(r))), nil
}

func (s *Server) injectIntoRun(run *Run, msg string) {
	s.expectReply(run.ID)
	s.enqueue(run.NodeID, proto.Command{Type: "message", RunID: run.ID, Message: msg})
}

func (s *Server) apiRunMessage(w http.ResponseWriter, r *http.Request) {
	run, err := s.runByID(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "run not found")
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := readJSON(r, &in); err != nil || strings.TrimSpace(in.Body) == "" {
		httpErr(w, 400, "body required")
		return
	}
	if run.Status != "running" {
		// Continue the conversation in a new chat run on the same machine.
		p, _ := s.projectByID(run.ProjectID)
		n, err := s.node(run.NodeID)
		if err != nil || p == nil || !n.Online {
			httpErr(w, 409, "run has ended and its machine is offline")
			return
		}
		id := newID()
		a := s.buildAssignment(id, "chat", p, nil, n, chatPrompt("human", in.Body))
		s.gpuSetup(a, n, nil, false)
		a.ResumeSession = run.SessionID
		if run.IssueID != nil {
			if i, err := s.issueByID(*run.IssueID); err == nil {
				a.Branch = i.Branch
			}
		}
		s.insertRun(id, p.ID, run.IssueID, n.ID, "chat", a.Prompt, nil)
		s.db.Exec(`UPDATE runs SET kind='chat' WHERE id=?`, id)
		s.expectReply(id)
		s.enqueue(n.ID, proto.Command{Type: "assign", Assignment: a})
		writeJSON(w, map[string]string{"run_id": id})
		return
	}
	s.injectIntoRun(run, "[Message from human]\n"+in.Body)
	writeJSON(w, map[string]string{"run_id": run.ID})
}

func (s *Server) apiRunCancel(w http.ResponseWriter, r *http.Request) {
	run, err := s.runByID(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "run not found")
		return
	}
	if run.Status != "running" {
		w.WriteHeader(204)
		return
	}
	n, err := s.node(run.NodeID)
	if err != nil || !n.Online {
		s.finishRun(run.ID, proto.RunFinish{Status: "cancelled", Error: "cancelled by human (machine offline)"})
	} else {
		s.enqueue(run.NodeID, proto.Command{Type: "cancel", RunID: run.ID})
	}
	w.WriteHeader(204)
}

// ---- nodes -----------------------------------------------------------------------

func (s *Server) apiNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.listNodes()
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	writeJSON(w, nzn(nodes))
}

func (s *Server) apiUpdateNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.node(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "machine not found")
		return
	}
	var in struct {
		Paused       *bool     `json:"paused"`
		Roles        *[]string `json:"roles"` // empty array = reset to runner-reported
		MaxConc      *int      `json:"max_conc"`
		ExtraCaps    *[]string `json:"extra_caps"`
		DisabledCaps *[]string `json:"disabled_caps"`
		Model        *string   `json:"model"`
		Name         *string   `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if in.Paused != nil {
		s.db.Exec(`UPDATE nodes SET paused=? WHERE id=?`, *in.Paused, n.ID)
	}
	if in.Roles != nil {
		v := ""
		if len(*in.Roles) > 0 {
			v = jsonStr(cleanList(*in.Roles))
		}
		s.db.Exec(`UPDATE nodes SET override_roles=? WHERE id=?`, v, n.ID)
	}
	if in.MaxConc != nil && *in.MaxConc >= 0 {
		s.db.Exec(`UPDATE nodes SET override_conc=? WHERE id=?`, *in.MaxConc, n.ID)
	}
	if in.ExtraCaps != nil {
		s.db.Exec(`UPDATE nodes SET extra_caps=? WHERE id=?`, jsonStr(cleanList(*in.ExtraCaps)), n.ID)
	}
	if in.DisabledCaps != nil {
		s.db.Exec(`UPDATE nodes SET disabled_caps=? WHERE id=?`, jsonStr(cleanList(*in.DisabledCaps)), n.ID)
	}
	if in.Model != nil {
		s.db.Exec(`UPDATE nodes SET model=? WHERE id=?`, strings.TrimSpace(*in.Model), n.ID)
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		s.db.Exec(`UPDATE nodes SET name=? WHERE id=?`, strings.TrimSpace(*in.Name), n.ID)
	}
	s.emit(event{Type: "nodes"})
	s.workAvailable()
	n, _ = s.node(n.ID)
	writeJSON(w, n)
}

func (s *Server) apiDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rows, _ := s.db.Query(`SELECT id FROM runs WHERE node_id=? AND status='running'`, id)
	var ids []string
	for rows != nil && rows.Next() {
		var x string
		rows.Scan(&x)
		ids = append(ids, x)
	}
	if rows != nil {
		rows.Close()
	}
	for _, x := range ids {
		s.finishRun(x, proto.RunFinish{Status: "lost", Error: "machine removed"})
	}
	s.db.Exec(`DELETE FROM node_projects WHERE node_id=?`, id)
	s.db.Exec(`DELETE FROM chat_sessions WHERE node_id=?`, id)
	s.db.Exec(`DELETE FROM nodes WHERE id=?`, id)
	s.emit(event{Type: "nodes"})
	w.WriteHeader(204)
}

func (s *Server) apiAssignNode(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	if _, err := s.node(r.PathValue("id")); err != nil {
		httpErr(w, 404, "machine not found")
		return
	}
	s.db.Exec(`INSERT OR IGNORE INTO node_projects(node_id,project_id) VALUES(?,?)`, r.PathValue("id"), p.ID)
	s.emit(event{Type: "nodes"})
	s.workAvailable()
	w.WriteHeader(204)
}

func (s *Server) apiUnassignNode(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	s.db.Exec(`DELETE FROM node_projects WHERE node_id=? AND project_id=?`, r.PathValue("id"), p.ID)
	s.emit(event{Type: "nodes"})
	w.WriteHeader(204)
}

// ---- repo browsing ---------------------------------------------------------------

func refParam(r *http.Request, rp *Repo) string {
	ref := r.URL.Query().Get("ref")
	if ref == "" || strings.HasPrefix(ref, "-") {
		ref = rp.DefaultBranch
	}
	return ref
}

func (s *Server) apiBranches(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	b, err := s.branches(rp.Key, rp.DefaultBranch)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if b == nil {
		b = []BranchInfo{}
	}
	writeJSON(w, b)
}

func (s *Server) apiTree(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	entries, err := s.lsTree(rp.Key, refParam(r, rp), r.URL.Query().Get("path"))
	if err != nil {
		httpErr(w, 404, err.Error())
		return
	}
	if entries == nil {
		entries = []TreeEntry{}
	}
	writeJSON(w, entries)
}

func (s *Server) apiBlob(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	path := strings.Trim(r.URL.Query().Get("path"), "/")
	spec := refParam(r, rp) + ":" + path
	data, err := s.git(rp.Key, nil, "cat-file", "blob", spec)
	if err != nil {
		httpErr(w, 404, err.Error())
		return
	}
	if r.URL.Query().Get("raw") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
		w.Write(data)
		return
	}
	out := map[string]any{"path": path, "size": len(data)}
	if bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1")) {
		var oid string
		var size int64
		for _, l := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(l, "oid sha256:") {
				oid = strings.TrimPrefix(l, "oid sha256:")
			}
			if strings.HasPrefix(l, "size ") {
				size, _ = strconv.ParseInt(strings.TrimPrefix(l, "size "), 10, 64)
			}
		}
		out["lfs"] = map[string]any{"oid": oid, "size": size, "href": "/git/" + rp.Key + ".git/info/lfs/objects/" + oid}
	} else if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		out["binary"] = true
	} else {
		if len(data) > 1<<20 {
			data = data[:1<<20]
			out["truncated"] = true
		}
		out["text"] = string(data)
	}
	writeJSON(w, out)
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	ref := r.URL.Query().Get("ref")
	if strings.HasPrefix(ref, "-") {
		ref = ""
	}
	l, err := s.gitLog(rp.Key, ref, 100)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if l == nil {
		l = []CommitInfo{}
	}
	writeJSON(w, l)
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

func (s *Server) apiCommit(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	sha := r.URL.Query().Get("sha")
	if !shaRe.MatchString(sha) {
		httpErr(w, 400, "bad sha")
		return
	}
	out, err := s.git(rp.Key, nil, "show", "--stat", "--patch", "--format=commit %H%nAuthor: %an <%ae>%nDate:   %ad%n%n%B", sha)
	if err != nil {
		httpErr(w, 404, err.Error())
		return
	}
	if len(out) > 800_000 {
		out = append(out[:800_000], []byte("\n… (truncated)")...)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(out)
}
