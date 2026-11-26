package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"multiclaude/internal/proto"
)

// Questions: an agent asks the humans something (ask_human). The question
// shows up in the web UI (sidebar badge, Questions page, project chat). The
// agent can wait for the answer inside the tool call; if it stops waiting the
// answer is delivered later — injected into its session while it runs, or, once
// the session has ended, added to its issue (a blocked issue is re-queued).

const questionSchema = `
CREATE TABLE IF NOT EXISTS questions(
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL,
  run_id TEXT NOT NULL,
  issue_id INTEGER,
  node_id TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL,
  question TEXT NOT NULL,
  context TEXT NOT NULL DEFAULT '',
  options TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'open', -- open | answered | dismissed | superseded
  answer TEXT NOT NULL DEFAULT '',
  answered_by TEXT NOT NULL DEFAULT '',
  delivered INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  answered_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS questions_status ON questions(status, id);
CREATE INDEX IF NOT EXISTS questions_run ON questions(run_id);
`

type Question struct {
	ID          int64    `json:"id"`
	ProjectID   int64    `json:"project_id"`
	RunID       string   `json:"run_id"`
	IssueID     *int64   `json:"issue_id"`
	NodeID      string   `json:"node_id"`
	Agent       string   `json:"agent"`
	Question    string   `json:"question"`
	Context     string   `json:"context"`
	Options     []string `json:"options"`
	Status      string   `json:"status"`
	Answer      string   `json:"answer"`
	AnsweredBy  string   `json:"answered_by"`
	CreatedAt   int64    `json:"created_at"`
	AnsweredAt  int64    `json:"answered_at"`
	ProjectSlug string   `json:"project_slug"`
	ProjectName string   `json:"project_name"`
	IssueNumber int      `json:"issue_number,omitempty"`
	IssueTitle  string   `json:"issue_title,omitempty"`
	RunStatus   string   `json:"run_status"`
	Waiting     bool     `json:"waiting"` // the agent is blocked in ask_human / wait_for_answer right now
}

const questionSelect = `SELECT q.id,q.project_id,q.run_id,q.issue_id,q.node_id,q.agent,q.question,q.context,q.options,q.status,q.answer,q.answered_by,q.created_at,q.answered_at,
  COALESCE(p.slug,''),COALESCE(p.name,''),COALESCE(i.number,0),COALESCE(i.title,''),COALESCE(r.status,'')
  FROM questions q LEFT JOIN projects p ON p.id=q.project_id LEFT JOIN issues i ON i.id=q.issue_id LEFT JOIN runs r ON r.id=q.run_id `

func scanQuestion(r interface{ Scan(...any) error }) (*Question, error) {
	q := &Question{}
	var opts string
	var issue sql.NullInt64
	err := r.Scan(&q.ID, &q.ProjectID, &q.RunID, &issue, &q.NodeID, &q.Agent, &q.Question, &q.Context, &opts, &q.Status, &q.Answer, &q.AnsweredBy, &q.CreatedAt, &q.AnsweredAt,
		&q.ProjectSlug, &q.ProjectName, &q.IssueNumber, &q.IssueTitle, &q.RunStatus)
	if issue.Valid {
		q.IssueID = &issue.Int64
	}
	q.Options = parseStrs(opts)
	q.Waiting = waiterFor(q.ID)
	return q, err
}

func (s *Server) question(id int64) (*Question, error) {
	return scanQuestion(s.db.QueryRow(questionSelect+`WHERE q.id=?`, id))
}

func (s *Server) openQuestionCount() int {
	var c int
	s.db.QueryRow(`SELECT COUNT(*) FROM questions WHERE status='open'`).Scan(&c)
	return c
}

// ---- waiting agents -------------------------------------------------------------

var (
	qwMu    sync.Mutex
	qwaiter = map[int64]chan struct{}{} // question id → closed when answered/dismissed
)

func waiterFor(id int64) bool {
	qwMu.Lock()
	defer qwMu.Unlock()
	_, ok := qwaiter[id]
	return ok
}

// waitAnswer blocks until the question is answered, the wait elapses or the
// caller goes away. It returns the (re-read) question.
func (s *Server) waitAnswer(ctx context.Context, id int64, d time.Duration) *Question {
	qwMu.Lock()
	ch, ok := qwaiter[id]
	if !ok {
		ch = make(chan struct{})
		qwaiter[id] = ch
	}
	qwMu.Unlock()
	s.emit(event{Type: "questions"})
	defer func() {
		qwMu.Lock()
		if qwaiter[id] == ch {
			delete(qwaiter, id)
		}
		qwMu.Unlock()
		s.emit(event{Type: "questions"})
	}()
	if q, err := s.question(id); err == nil && q.Status != "open" {
		return q
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
	q, _ := s.question(id)
	return q
}

// wake releases a waiting agent; reports whether one was waiting.
func wake(id int64) bool {
	qwMu.Lock()
	defer qwMu.Unlock()
	ch, ok := qwaiter[id]
	if ok {
		close(ch)
		delete(qwaiter, id)
	}
	return ok
}

// maxWait is how long one tool call may block. Runners that support long
// calls announce it; older ones time out after two minutes.
func maxWait(r *http.Request) time.Duration {
	if v, err := strconv.Atoi(r.Header.Get("X-MC-Max-Wait")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 100 * time.Second
}

// ---- agent tools ------------------------------------------------------------------

// askHuman implements ask_human and wait_for_answer.
func (s *Server) askHuman(ctx context.Context, limit time.Duration, run *Run, tool string, args map[string]any, callID string) (string, error) {
	waitMin := 10.0
	if v, ok := args["wait_minutes"].(float64); ok && v >= 0 {
		waitMin = v
	}
	var id int64
	if tool == "ask_human" && callID != "" {
		// The runner retried this call (e.g. the server restarted while the agent waited): keep waiting on the same question.
		s.db.QueryRow(`SELECT id FROM questions WHERE call_id=?`, callID).Scan(&id)
	}
	if tool == "ask_human" && id == 0 {
		text := strings.TrimSpace(fmt.Sprint(args["question"]))
		if args["question"] == nil || text == "" {
			return "", fmt.Errorf("question is required")
		}
		var opts []string
		if l, ok := args["options"].([]any); ok {
			for _, o := range l {
				if o := strings.TrimSpace(fmt.Sprint(o)); o != "" {
					opts = append(opts, o)
				}
			}
		}
		ctxText, _ := args["context"].(string)
		res, err := s.db.Exec(`INSERT INTO questions(project_id,run_id,issue_id,node_id,agent,question,context,options,created_at,call_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			run.ProjectID, run.ID, run.IssueID, run.NodeID, run.handle(), text, strings.TrimSpace(ctxText), jsonStr(nz(opts)), now(), callID)
		if err != nil {
			return "", err
		}
		id, _ = res.LastInsertId()
		if run.IssueID != nil {
			// A retry of the issue asked again: the older, unanswered questions are replaced.
			s.db.Exec(`UPDATE questions SET status='superseded', answered_at=? WHERE issue_id=? AND status='open' AND id<>?`, now(), *run.IssueID, id)
		}
		where := ""
		if run.IssueNumber > 0 {
			where = fmt.Sprintf(" (#%d)", run.IssueNumber)
		}
		s.postMessage(run.ProjectID, run.handle(), "", fmt.Sprintf("❓ Question Q%d%s: %s\n\nAnswer it on the Questions page.", id, where, text))
		s.emit(event{Type: "questions"})
	} else if tool == "wait_for_answer" {
		v, _ := args["id"].(float64)
		id = int64(v)
		q, err := s.question(id)
		sameIssue := err == nil && q.IssueID != nil && run.IssueID != nil && *q.IssueID == *run.IssueID
		if err != nil || (q.RunID != run.ID && !sameIssue) {
			return "", fmt.Errorf("question Q%d not found for this session", id)
		}
	}
	d := time.Duration(waitMin * float64(time.Minute))
	if d > limit {
		d = limit
	}
	var q *Question
	if d > 0 {
		q = s.waitAnswer(ctx, id, d)
	} else {
		q, _ = s.question(id)
	}
	if q == nil {
		return "", fmt.Errorf("question Q%d vanished", id)
	}
	switch q.Status {
	case "answered":
		s.db.Exec(`UPDATE questions SET delivered=1 WHERE id=?`, id)
		return fmt.Sprintf("Answer to Q%d from %s:\n%s", id, q.AnsweredBy, q.Answer), nil
	case "dismissed":
		s.db.Exec(`UPDATE questions SET delivered=1 WHERE id=?`, id)
		msg := fmt.Sprintf("%s dismissed Q%d without an answer: decide yourself and state your assumption in your summary.", q.AnsweredBy, id)
		if q.Answer != "" {
			msg += " Note: " + q.Answer
		}
		return msg, nil
	}
	waited := ""
	if d > 0 {
		waited = fmt.Sprintf(" after waiting %s", d.Round(time.Second))
	}
	return fmt.Sprintf(`No answer to Q%d yet%s. The question stays open in the web UI. Options:
- call wait_for_answer with id %d to keep waiting (if nothing else is useful meanwhile),
- continue with work that doesn't depend on the answer, or on a sensible assumption — a later answer is injected into this conversation as a message,
- if you really cannot proceed: block_issue; when the human answers, the answer is added to the issue and it goes back to the queue.`, id, waited, id), nil
}

// ---- human side -------------------------------------------------------------------

func (s *Server) apiQuestions(w http.ResponseWriter, r *http.Request) {
	where := `WHERE q.status='open' ORDER BY q.id DESC`
	if r.URL.Query().Get("status") == "recent" {
		where = `WHERE q.status IN ('answered','dismissed') ORDER BY q.answered_at DESC LIMIT 30`
	}
	rows, err := s.db.Query(questionSelect + where)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	out := []*Question{}
	for rows.Next() {
		if q, err := scanQuestion(rows); err == nil {
			out = append(out, q)
		}
	}
	rows.Close()
	writeJSON(w, out)
}

func (s *Server) apiAnswerQuestion(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var in struct {
		Answer  string `json:"answer"`
		Dismiss bool   `json:"dismiss"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	in.Answer = strings.TrimSpace(in.Answer)
	if !in.Dismiss && in.Answer == "" {
		httpErr(w, 400, "answer required")
		return
	}
	by := "human"
	if u := userFrom(r); u != nil {
		by = u.Username
	}
	status := "answered"
	if in.Dismiss {
		status = "dismissed"
	}
	res, _ := s.db.Exec(`UPDATE questions SET status=?, answer=?, answered_by=?, answered_at=? WHERE id=? AND status='open'`, status, in.Answer, by, now(), id)
	if n, _ := res.RowsAffected(); n == 0 {
		httpErr(w, 409, "question is not open any more")
		return
	}
	q, err := s.question(id)
	if err != nil {
		httpErr(w, 404, "not found")
		return
	}
	delivery := s.deliverAnswer(q)
	s.emit(event{Type: "questions"})
	writeJSON(w, map[string]any{"question": q, "delivery": delivery})
}

// deliverAnswer gets the answer to the agent, however it is reachable now.
func (s *Server) deliverAnswer(q *Question) string {
	verb := "answered"
	if q.Status == "dismissed" {
		verb = "dismissed"
	}
	chat := fmt.Sprintf("💬 %s %s Q%d (%q)", q.AnsweredBy, verb, q.ID, clip(q.Question, 120))
	if q.Answer != "" {
		chat += ":\n" + q.Answer
	}
	s.postMessage(q.ProjectID, "human", "", chat)

	if wake(q.ID) {
		return "delivered to the waiting agent"
	}
	run, err := s.runByID(q.RunID)
	if (err != nil || run.Status != "running") && q.IssueID != nil {
		// The asking session ended, but a later session may be working on the issue now.
		var cur string
		s.db.QueryRow(`SELECT run_id FROM issues WHERE id=?`, *q.IssueID).Scan(&cur)
		if cur != "" {
			if r2, err2 := s.runByID(cur); err2 == nil && r2.Status == "running" {
				run, err = r2, nil
			}
		}
	}
	if err == nil && run.Status == "running" {
		s.db.Exec(`UPDATE questions SET delivered=1 WHERE id=?`, q.ID)
		msg := fmt.Sprintf("[Answer from %s to your question Q%d: %q]\n%s", q.AnsweredBy, q.ID, q.Question, firstNonEmpty(q.Answer, "(dismissed — decide yourself)"))
		s.expectReply(run.ID)
		s.enqueue(run.NodeID, proto.Command{Type: "message", RunID: run.ID, Message: msg})
		return "injected into the running session"
	}
	if q.IssueID != nil {
		i, err := s.issueByID(*q.IssueID)
		if err == nil {
			s.addComment(i.ID, q.AnsweredBy, fmt.Sprintf("💬 Answer to question Q%d from %s:\n\n> %s\n\n%s", q.ID, q.Agent, strings.ReplaceAll(q.Question, "\n", "\n> "), firstNonEmpty(q.Answer, "(dismissed — decide yourself)")))
			s.db.Exec(`UPDATE questions SET delivered=1 WHERE id=?`, q.ID)
			note := "added to issue #" + strconv.Itoa(i.Number)
			if i.Status == "blocked" {
				s.db.Exec(`UPDATE issues SET status='todo', updated_at=? WHERE id=?`, now(), i.ID)
				note += ", which goes back to To do"
				s.workAvailable()
			}
			s.issueChanged(i.ProjectID)
			return note
		}
	}
	// A chat session that has ended: continue the conversation in the chat.
	if p, err := s.projectByID(q.ProjectID); err == nil {
		if n, err := s.node(q.NodeID); err == nil {
			s.db.Exec(`UPDATE questions SET delivered=1 WHERE id=?`, q.ID)
			return s.deliverChat(p, []*Node{n}, q.AnsweredBy, fmt.Sprintf("Answer to the question Q%d you asked earlier (%q):\n%s", q.ID, q.Question, firstNonEmpty(q.Answer, "(dismissed — decide yourself)")))
		}
	}
	return "posted to the project chat"
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// openIssueQuestions lists unanswered questions of an issue for the agent prompt.
func (s *Server) openIssueQuestions(issueID int64) string {
	rows, err := s.db.Query(`SELECT id, question FROM questions WHERE issue_id=? AND status='open' ORDER BY id`, issueID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id int64
		var text string
		rows.Scan(&id, &text)
		fmt.Fprintf(&b, "- Q%d: %s\n", id, clip(text, 300))
	}
	return b.String()
}

func (s *Server) openQuestionsByIssue(pid int64) map[int64]int {
	out := map[int64]int{}
	rows, err := s.db.Query(`SELECT issue_id, COUNT(*) FROM questions WHERE project_id=? AND status='open' AND issue_id IS NOT NULL GROUP BY issue_id`, pid)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		rows.Scan(&id, &n)
		out[id] = n
	}
	return out
}
