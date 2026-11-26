package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// ---- scalable issue queries -------------------------------------------------

type issueQuery struct {
	Status   string // "" (all) | "open" (not done) | a status | comma-separated statuses
	Q        string // free text; "#123" or "123" also matches the number
	Label    string
	Requires string
	Assignee string
	Order    string // board (default) | updated | created | priority | number
	Limit    int
	Offset   int
}

func (q *issueQuery) where(pid int64) (string, []any) {
	w := []string{"project_id=?"}
	args := []any{pid}
	switch st := strings.TrimSpace(q.Status); {
	case st == "":
	case st == "open":
		w = append(w, "status<>'done'")
	default:
		var ph []string
		for _, s := range strings.Split(st, ",") {
			if s = strings.TrimSpace(s); validStatus[s] {
				ph = append(ph, "?")
				args = append(args, s)
			}
		}
		if len(ph) > 0 {
			w = append(w, "status IN ("+strings.Join(ph, ",")+")")
		}
	}
	if t := strings.TrimSpace(q.Q); t != "" {
		if n, err := strconv.Atoi(strings.TrimPrefix(t, "#")); err == nil {
			w = append(w, "(number=? OR title LIKE ?)")
			args = append(args, n, "%"+t+"%")
		} else {
			for _, word := range strings.Fields(t) { // every word must match somewhere
				l := "%" + word + "%"
				w = append(w, "(title LIKE ? OR body LIKE ? OR labels LIKE ? OR assignee LIKE ?)")
				args = append(args, l, l, l, l)
			}
		}
	}
	jsonHas := func(col, v string) {
		if v = strings.TrimSpace(v); v != "" {
			w = append(w, "EXISTS (SELECT 1 FROM json_each("+col+") WHERE lower(value)=lower(?))")
			args = append(args, v)
		}
	}
	jsonHas("labels", q.Label)
	jsonHas("requires", q.Requires)
	if a := strings.TrimSpace(q.Assignee); a != "" {
		w = append(w, "assignee=?")
		args = append(args, a)
	}
	return strings.Join(w, " AND "), args
}

func (q *issueQuery) orderBy() string {
	switch q.Order {
	case "updated":
		return "updated_at DESC, number DESC"
	case "created":
		return "created_at DESC, number DESC"
	case "priority":
		return "priority, sort, number"
	case "number":
		return "number DESC"
	}
	return "sort, number"
}

func (s *Server) queryIssues(pid int64, q issueQuery) ([]*Issue, int, error) {
	where, args := q.where(pid)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM issues WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sql := `SELECT ` + issueCols + ` FROM issues WHERE ` + where + ` ORDER BY ` + q.orderBy()
	if q.Limit > 0 {
		sql += ` LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	}
	rows, err := s.db.Query(sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Issue{}
	for rows.Next() {
		if i, err := scanIssue(rows); err == nil {
			out = append(out, i)
		}
	}
	return out, total, nil
}

func queryFromReq(r *http.Request) issueQuery {
	v := r.URL.Query()
	atoi := func(k string) int { n, _ := strconv.Atoi(v.Get(k)); return n }
	return issueQuery{Status: v.Get("status"), Q: v.Get("q"), Label: v.Get("label"), Requires: v.Get("requires"),
		Assignee: v.Get("assignee"), Order: v.Get("order"), Limit: atoi("limit"), Offset: atoi("offset")}
}

type boardColumn struct {
	Items []*Issue `json:"items"`
	Total int      `json:"total"`
}

// apiBoard returns every board column capped to a page, so the board stays
// fast with thousands of issues. Done shows only the most recent ones (the
// rest is the archive).
func (s *Server) apiBoard(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	base := queryFromReq(r)
	per, _ := strconv.Atoi(r.URL.Query().Get("per"))
	if per <= 0 || per > 1000 {
		per = 150
	}
	done, _ := strconv.Atoi(r.URL.Query().Get("done"))
	if done <= 0 || done > 500 {
		done = 25
	}
	cols := map[string]boardColumn{}
	for st := range validStatus {
		q := base
		q.Status, q.Limit, q.Offset, q.Order = st, per, 0, "board"
		if st == "done" {
			q.Limit, q.Order = done, "updated"
		}
		items, total, err := s.queryIssues(p.ID, q)
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		cols[st] = boardColumn{items, total}
	}
	open := s.openQuestionsByIssue(p.ID)
	for _, c := range cols {
		for _, i := range c.Items {
			i.OpenQuestions = open[i.ID]
		}
	}
	writeJSON(w, map[string]any{"columns": cols, "agents": s.withActivity(s.projectAgents(p.ID))})
}

// ---- live activity: what each running agent is doing right now --------------

type Activity struct {
	Text  string `json:"text"`
	Kind  string `json:"kind"` // tool | text | result | start
	Tool  string `json:"tool,omitempty"`
	TS    int64  `json:"ts"`
	Tools int    `json:"tools"` // tool calls so far
	Edits int    `json:"edits"` // file edits so far
}

var (
	actMu sync.Mutex
	acts  = map[string]*Activity{}
)

func activityOf(runID string) *Activity {
	actMu.Lock()
	defer actMu.Unlock()
	if a := acts[runID]; a != nil {
		c := *a
		return &c
	}
	return nil
}

func forgetActivity(runID string) {
	actMu.Lock()
	delete(acts, runID)
	actMu.Unlock()
}

func (s *Server) withActivity(runs []*Run) []*Run {
	if runs == nil {
		return []*Run{}
	}
	for _, r := range runs {
		if r.Status == "running" {
			r.Activity = activityOf(r.ID)
		}
		r.Prompt = ""
	}
	return runs
}

var editTools = map[string]bool{"Edit": true, "MultiEdit": true, "Write": true, "NotebookEdit": true}

// toolLine renders a tool call as one human-readable line.
func toolLine(name string, input map[string]any) string {
	str := func(k string) string { v, _ := input[k].(string); return v }
	short := func(s string, n int) string {
		s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
		if len(s) > n {
			s = s[:n] + "…"
		}
		return s
	}
	name = strings.TrimPrefix(name, "mcp__multiclaude__")
	switch name {
	case "Bash":
		return "$ " + short(str("command"), 120)
	case "Edit", "MultiEdit":
		return "✎ editing " + str("file_path")
	case "Write":
		return "✎ writing " + str("file_path")
	case "Read":
		return "📖 reading " + str("file_path")
	case "Grep", "Glob":
		return "🔍 searching " + short(str("pattern"), 80)
	case "Task", "Agent":
		return "🧩 subagent: " + short(str("description"), 100)
	case "TodoWrite":
		return "☑ updating plan"
	case "WebFetch", "WebSearch":
		return "🌐 " + short(str("url")+str("query"), 100)
	case "post_chat":
		return "💬 messaging " + firstNonEmpty(str("to"), "everyone")
	case "create_issue":
		return "➕ creating issue: " + short(str("title"), 90)
	case "complete_work":
		return "✔ completing work"
	case "review_verdict":
		return "⚖ review verdict: " + str("verdict")
	case "ask_human":
		return "❓ asking the humans: " + short(str("question"), 90) + " (waiting for an answer)"
	case "wait_for_answer":
		return "❓ waiting for a human answer"
	case "block_issue":
		return "🚧 blocking: " + short(str("reason"), 90)
	}
	for _, k := range []string{"file_path", "path", "pattern", "description", "title", "query", "url"} {
		if v := str(k); v != "" {
			return name + " " + short(v, 100)
		}
	}
	return name
}

// trackActivity updates the live activity of a run from one stream-json event
// and broadcasts a small "activity" event (cheap for the board to consume).
func (s *Server) trackActivity(run *Run, typ string, data json.RawMessage) {
	var ev struct {
		Subtype string `json:"subtype"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Text  string         `json:"text"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	actMu.Lock()
	a := acts[run.ID]
	if a == nil {
		a = &Activity{Kind: "start", Text: "starting…"}
		acts[run.ID] = a
	}
	changed := false
	switch typ {
	case "system":
		if json.Unmarshal(data, &ev) == nil && ev.Subtype == "init" {
			a.Kind, a.Text, changed = "start", "session started", true
		}
	case "assistant":
		if json.Unmarshal(data, &ev) == nil {
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "tool_use":
					a.Kind, a.Tool, a.Text = "tool", strings.TrimPrefix(c.Name, "mcp__multiclaude__"), toolLine(c.Name, c.Input)
					a.Tools++
					if editTools[c.Name] {
						a.Edits++
					}
					changed = true
				case "text":
					if t := strings.TrimSpace(c.Text); t != "" {
						line := strings.SplitN(t, "\n", 2)[0]
						if len(line) > 140 {
							line = line[:140] + "…"
						}
						a.Kind, a.Tool, a.Text = "text", "", "💭 "+line
						changed = true
					}
				}
			}
		}
	case "result":
		a.Kind, a.Tool, a.Text, changed = "result", "", "turn finished", true
	case "human":
		a.Kind, a.Tool, a.Text, changed = "text", "", "📨 received a message", true
	}
	if changed {
		a.TS = now()
	}
	snap := *a
	actMu.Unlock()
	if changed {
		s.emit(event{Type: "activity", RunID: run.ID, ProjectID: run.ProjectID, Data: snap})
	}
}
