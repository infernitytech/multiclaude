package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"multiclaude/internal/proto"
)

// agentCall handles MCP tool calls coming from Claude sessions (via the runner's MCP bridge).
func (s *Server) agentCall(w http.ResponseWriter, r *http.Request) {
	var c proto.AgentCall
	if err := readJSON(r, &c); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if run, err := s.runByID(c.RunID); err != nil || run.NodeID != nodeFrom(r) {
		writeJSON(w, proto.AgentResult{Text: "unknown run for this machine", IsError: true})
		return
	}
	// A retry of a call the server already carried out (the runner retries
	// while the server restarts) gets the stored result instead of running twice.
	callID := r.Header.Get("X-MC-Call-ID")
	if callID != "" {
		var res proto.AgentResult
		if s.db.QueryRow(`SELECT text, is_error FROM agent_calls WHERE id=?`, callID).Scan(&res.Text, &res.IsError) == nil {
			writeJSON(w, res)
			return
		}
	}
	var text string
	var err error
	if c.Tool == "ask_human" || c.Tool == "wait_for_answer" {
		run, _ := s.runByID(c.RunID)
		var args map[string]any
		json.Unmarshal(c.Args, &args)
		if args == nil {
			args = map[string]any{}
		}
		text, err = s.askHuman(r.Context(), maxWait(r), run, c.Tool, args, callID)
	} else {
		text, err = s.dispatchTool(c)
	}
	res := proto.AgentResult{Text: text}
	if err != nil {
		res = proto.AgentResult{Text: err.Error(), IsError: true}
	}
	if callID != "" && r.Context().Err() == nil {
		s.db.Exec(`INSERT OR IGNORE INTO agent_calls(id, text, is_error, created_at) VALUES(?,?,?,?)`, callID, res.Text, res.IsError, now())
	}
	writeJSON(w, res)
}

const agentCallSchema = `
CREATE TABLE IF NOT EXISTS agent_calls(
  id TEXT PRIMARY KEY,
  text TEXT NOT NULL,
  is_error INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
`

var lastPrune time.Time

// pruneAgentCalls forgets results of tool calls older than a day.
func pruneAgentCalls(s *Server) {
	if time.Since(lastPrune) < time.Hour {
		return
	}
	lastPrune = time.Now()
	s.db.Exec(`DELETE FROM agent_calls WHERE created_at < ?`, now()-86_400_000)
}

func (s *Server) dispatchTool(c proto.AgentCall) (string, error) {
	run, err := s.runByID(c.RunID)
	if err != nil {
		return "", fmt.Errorf("unknown run %q", c.RunID)
	}
	p, err := s.projectByID(run.ProjectID)
	if err != nil {
		return "", err
	}
	me := run.NodeName
	var args map[string]json.RawMessage
	if len(c.Args) > 0 {
		json.Unmarshal(c.Args, &args)
	}
	str := func(k string) string {
		var v string
		json.Unmarshal(args[k], &v)
		return v
	}
	intv := func(k string) int {
		var v int
		json.Unmarshal(args[k], &v)
		return v
	}
	ownIssue := func() (*Issue, error) {
		if run.IssueID == nil {
			return nil, fmt.Errorf("this session has no assigned issue")
		}
		return s.issueByID(*run.IssueID)
	}

	switch c.Tool {
	case "list_issues":
		q := issueQuery{Status: firstNonEmpty(str("status"), "open"), Q: str("query"), Label: str("label"), Requires: str("requires"), Order: str("order"), Limit: intv("limit"), Offset: intv("offset")}
		if q.Status == "all" {
			q.Status = ""
		}
		if q.Limit <= 0 || q.Limit > 200 {
			q.Limit = 60
		}
		if q.Order == "" && q.Status == "done" {
			q.Order = "updated"
		}
		issues, total, err := s.queryIssues(p.ID, q)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		if total > q.Offset+len(issues) {
			fmt.Fprintf(&b, "Showing %d–%d of %d matching issues (narrow with status/query/label/requires, or page with offset).\n", q.Offset+1, q.Offset+len(issues), total)
		}
		for _, i := range issues {
			fmt.Fprintf(&b, "#%d [%s] (%s) %s", i.Number, i.Status, priorityNames[clampPrio(i.Priority)], i.Title)
			if i.Assignee != "" && (i.Status == "in_progress" || i.Status == "review") {
				fmt.Fprintf(&b, " — %s", i.Assignee)
			}
			if len(i.Requires) > 0 {
				fmt.Fprintf(&b, " requires:%s", strings.Join(i.Requires, ","))
			}
			if len(i.DependsOn) > 0 {
				fmt.Fprintf(&b, " depends_on:%v", i.DependsOn)
			}
			b.WriteString("\n")
		}
		if b.Len() == 0 {
			return "No issues match.", nil
		}
		return b.String(), nil

	case "get_issue":
		i, err := s.issueByNumber(p.ID, intv("number"))
		if err != nil {
			return "", fmt.Errorf("issue #%d not found", intv("number"))
		}
		return fmt.Sprintf("Status: %s | Branch: %s | Assignee: %s\n%s", i.Status, i.Branch, i.Assignee, s.issueContext(i)), nil

	case "create_issue":
		var in issueInput
		json.Unmarshal(c.Args, &in)
		i, err := s.createIssue(p, in, me)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Created issue #%d (%s).", i.Number, i.Status) + s.schedulabilityNote(p, i), nil

	case "update_issue":
		i, err := s.issueByNumber(p.ID, intv("number"))
		if err != nil {
			return "", fmt.Errorf("issue #%d not found", intv("number"))
		}
		var in issueInput
		json.Unmarshal(c.Args, &in)
		if in.Status != nil && *in.Status != i.Status {
			if i.RunID == run.ID {
				return "", fmt.Errorf("use complete_work / block_issue / review_verdict to change the status of your own assignment")
			}
			if i.RunID != "" {
				return "", fmt.Errorf("#%d is being worked on by another agent right now; its status can't be changed (message that agent with post_chat, or comment_issue)", i.Number)
			}
			agentMoves := map[string]bool{"backlog": true, "todo": true, "blocked": true}
			if !agentMoves[i.Status] || !agentMoves[*in.Status] {
				return "", fmt.Errorf("agents may only move issues between backlog, todo and blocked (#%d is %s). Claiming, review and done are handled by the server", i.Number, i.Status)
			}
		}
		u, err := s.updateIssue(p, i, in, me)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Updated #%d.", i.Number) + s.schedulabilityNote(p, u), nil

	case "comment_issue":
		i, err := s.issueByNumber(p.ID, intv("number"))
		if err != nil {
			return "", fmt.Errorf("issue #%d not found", intv("number"))
		}
		s.addComment(i.ID, me, str("body"))
		s.issueChanged(p.ID)
		return "Comment added.", nil

	case "complete_work":
		i, err := ownIssue()
		if err != nil || run.Kind != "work" {
			return "", fmt.Errorf("complete_work is only for work assignments")
		}
		s.db.Exec(`UPDATE runs SET completed=1 WHERE id=?`, run.ID)
		s.addComment(i.ID, me, "✔️ Work completed:\n\n"+str("summary"))
		s.issueChanged(p.ID)
		return "Recorded. Make sure all commits are pushed to origin/" + i.Branch + ", then end your session (the runner also pushes leftovers).", nil

	case "block_issue":
		i, err := ownIssue()
		if err != nil {
			return "", err
		}
		s.db.Exec(`UPDATE issues SET status='blocked', run_id='', updated_at=? WHERE id=?`, now(), i.ID)
		s.addComment(i.ID, me, "🚧 Blocked: "+str("reason"))
		s.postMessage(p.ID, me, "", fmt.Sprintf("🚧 #%d %s is blocked: %s", i.Number, i.Title, str("reason")))
		s.issueChanged(p.ID)
		return "Issue marked as blocked; a human will look at it. End your session now.", nil

	case "review_verdict":
		i, err := ownIssue()
		if err != nil || run.Kind != "review" {
			return "", fmt.Errorf("review_verdict is only for review assignments")
		}
		v := str("verdict")
		if v != "approve" && v != "request_changes" {
			return "", fmt.Errorf(`verdict must be "approve" or "request_changes"`)
		}
		s.db.Exec(`UPDATE runs SET verdict=? WHERE id=?`, v, run.ID)
		icon := "👍 Approved"
		if v == "request_changes" {
			icon = "🔁 Changes requested"
		}
		if c := strings.TrimSpace(str("comment")); c != "" {
			icon += ":\n\n" + c
		}
		s.addComment(i.ID, me, icon)
		s.issueChanged(p.ID)
		return "Verdict recorded. Push any final commits and end your session; the server acts on the verdict when the session ends.", nil

	case "post_chat":
		return s.postChatFromAgent(p, run, str("to"), str("message"))

	case "read_chat":
		limit := intv("limit")
		if limit <= 0 || limit > 200 {
			limit = 30
		}
		msgs, _ := s.messages(p.ID, limit)
		var b strings.Builder
		for _, m := range msgs {
			to := ""
			if m.Target != "" && !strings.HasPrefix(m.Target, "reply:") {
				if n, err := s.node(m.Target); err == nil {
					to = " → " + n.Name
				}
			}
			fmt.Fprintf(&b, "[%s] %s%s: %s\n", time.UnixMilli(m.CreatedAt).Format("01-02 15:04"), m.Author, to, m.Body)
		}
		if b.Len() == 0 {
			return "The chat is empty.", nil
		}
		return b.String(), nil

	case "list_artifacts":
		issue := ""
		if n := intv("issue"); n > 0 {
			issue = strconv.Itoa(n)
		}
		list, total, size := s.listArtifacts(p.ID, str("query"), str("prefix"), issue, 100)
		if total == 0 {
			return "No matching artifacts. Upload with: multiclaude artifact put <file-or-dir> [name] -m \"description\"", nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d artifact(s), %s in total%s:\n", total, humanBytes(size), map[bool]string{true: " (newest 100 shown)", false: ""}[total > len(list)])
		for _, a := range list {
			fmt.Fprintf(&b, "- %s  %s  %s by %s", a.Name, humanBytes(a.Size), time.UnixMilli(a.CreatedAt).Format("2006-01-02 15:04"), a.UploadedBy)
			if a.IssueNumber > 0 {
				fmt.Fprintf(&b, " (#%d)", a.IssueNumber)
			}
			if a.Description != "" {
				b.WriteString(" — " + a.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("Download: multiclaude artifact get <name> [dest]")
		return b.String(), nil

	case "list_agents":
		nodes, _ := s.listNodes()
		var b strings.Builder
		for _, n := range nodes {
			state := "offline"
			if n.Online {
				state = "online"
			}
			if n.Paused {
				state += ", paused"
			}
			fmt.Fprintf(&b, "%s (%s) — %s; caps: %s; roles: %s; slots %d/%d\n", n.Name, state, hwSummary(n),
				strings.Join(n.AllCaps(), ","), strings.Join(n.Roles, ","), len(n.Active), n.MaxConc)
			for _, r := range n.Active {
				you := ""
				if r.ID == run.ID {
					you = " (you)"
				}
				fmt.Fprintf(&b, "   • agent %s%s — %s [project %s]\n", agentHandle(n.Name, r.ID), you, r.activity(), r.ProjectSlug)
			}
		}
		b.WriteString("\nMessage an agent with post_chat to=\"<agent>\" (e.g. machine/abc123), all agents of a machine with to=\"<machine>\", or every agent on this project by omitting \"to\".\n")
		return b.String(), nil
	}
	return "", fmt.Errorf("unknown tool %q", c.Tool)
}

func (s *Server) messages(pid int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT id,project_id,author,target,body,created_at FROM (SELECT * FROM messages WHERE project_id=? ORDER BY id DESC LIMIT ?) ORDER BY id`, pid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.ProjectID, &m.Author, &m.Target, &m.Body, &m.CreatedAt)
		out = append(out, m)
	}
	return out, nil
}

func (s *Server) listNodes() ([]*Node, error) {
	rows, err := s.db.Query(`SELECT ` + nodeCols + ` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var nodes []*Node
	for rows.Next() {
		if n, err := scanNode(rows); err == nil {
			nodes = append(nodes, n)
		}
	}
	rows.Close()
	for _, n := range nodes {
		n.Projects = s.nodeProjects(n.ID)
		n.Load = loadOf(n.ID)
		n.UpdatingTo = drainingTo(n.ID)
		n.SelfUpdate = selfUpdateOf(n.ID)
		n.Active = []Run{}
		rr, err := s.db.Query(runSelect+`WHERE r.node_id=? AND r.status='running' ORDER BY r.started_at`, n.ID)
		if err == nil {
			for rr.Next() {
				if r, err := scanRun(rr); err == nil {
					r.Prompt = ""
					r.Activity = activityOf(r.ID)
					n.Active = append(n.Active, *r)
				}
			}
			rr.Close()
		}
	}
	return nodes, nil
}
