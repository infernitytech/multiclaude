package server

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type issueInput struct {
	Title     *string          `json:"title"`
	Body      *string          `json:"body"`
	Status    *string          `json:"status"`
	Priority  *json.RawMessage `json:"priority"`
	Labels    *[]string        `json:"labels"`
	Requires  *[]string        `json:"requires"`
	DependsOn *[]int           `json:"depends_on"`
	Sort      *float64         `json:"sort"`
	Model     *string          `json:"model"`
}

func parsePriority(raw *json.RawMessage) (int, bool) {
	if raw == nil {
		return 0, false
	}
	var n int
	if json.Unmarshal(*raw, &n) == nil && n >= 0 && n <= 3 {
		return n, true
	}
	var s string
	if json.Unmarshal(*raw, &s) == nil {
		s = strings.ToLower(strings.TrimSpace(s))
		if p, ok := priorityFromName(s); ok {
			return p, true
		}
		if v, err := strconv.Atoi(s); err == nil && v >= 0 && v <= 3 {
			return v, true
		}
	}
	return 0, false
}

func cleanList(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range in {
		x = strings.TrimSpace(x)
		if x != "" && !seen[strings.ToLower(x)] {
			seen[strings.ToLower(x)] = true
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) createIssue(p *Project, in issueInput, author string) (*Issue, error) {
	if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
		return nil, fmt.Errorf("title is required")
	}
	status := "todo"
	if in.Status != nil && validStatus[*in.Status] {
		status = *in.Status
	}
	if status == "in_progress" || status == "review" || status == "done" {
		status = "todo"
	}
	prio := 2
	if p, ok := parsePriority(in.Priority); ok {
		prio = p
	}
	body := ""
	if in.Body != nil {
		body = *in.Body
	}
	var labels, req []string
	var deps []int
	if in.Labels != nil {
		labels = cleanList(*in.Labels)
	}
	if in.Requires != nil {
		req = cleanList(*in.Requires)
	}
	if in.DependsOn != nil {
		deps = *in.DependsOn
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var num int
	if err := tx.QueryRow(`UPDATE projects SET next_number=next_number+1 WHERE id=? RETURNING next_number-1`, p.ID).Scan(&num); err != nil {
		return nil, err
	}
	var maxSort float64
	tx.QueryRow(`SELECT COALESCE(MAX(sort),0) FROM issues WHERE project_id=?`, p.ID).Scan(&maxSort)
	t := now()
	model := ""
	if in.Model != nil {
		model = strings.TrimSpace(*in.Model)
	}
	res, err := tx.Exec(`INSERT INTO issues(project_id,number,title,body,status,priority,labels,requires,depends_on,created_by,created_at,updated_at,sort,model)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, num, strings.TrimSpace(*in.Title), body, status, prio,
		jsonStr(nz(labels)), jsonStr(nz(req)), jsonStr(nzi(deps)), author, t, t, maxSort+1, model)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	s.issueChanged(p.ID)
	s.workAvailable()
	return s.issueByID(id)
}

func nz(x []string) []string {
	if x == nil {
		return []string{}
	}
	return x
}
func nzi(x []int) []int {
	if x == nil {
		return []int{}
	}
	return x
}

// updateIssue applies changes. Moving an issue out of an active state cancels
// the run working on it.
func (s *Server) updateIssue(p *Project, i *Issue, in issueInput, author string) (*Issue, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, v any) { sets = append(sets, col+"=?"); args = append(args, v) }
	if in.Title != nil && strings.TrimSpace(*in.Title) != "" {
		add("title", strings.TrimSpace(*in.Title))
	}
	if in.Body != nil {
		add("body", *in.Body)
	}
	if pr, ok := parsePriority(in.Priority); ok {
		add("priority", pr)
	}
	if in.Labels != nil {
		add("labels", jsonStr(cleanList(*in.Labels)))
	}
	if in.Model != nil {
		add("model", strings.TrimSpace(*in.Model))
	}
	if in.Requires != nil {
		add("requires", jsonStr(cleanList(*in.Requires)))
	}
	if in.DependsOn != nil {
		add("depends_on", jsonStr(nzi(*in.DependsOn)))
	}
	if in.Sort != nil {
		add("sort", *in.Sort)
	}
	if in.Status != nil && *in.Status != i.Status {
		st := *in.Status
		if !validStatus[st] {
			return nil, fmt.Errorf("invalid status %q", st)
		}
		add("status", st)
		if i.RunID != "" {
			s.cancelIssueRun(i)
			add("run_id", "")
		}
		if st == "todo" || st == "backlog" {
			add("attempts", 0)
		}
		s.addComment(i.ID, author, fmt.Sprintf("moved %s → %s", i.Status, st))
	}
	if len(sets) == 0 {
		return i, nil
	}
	add("updated_at", now())
	args = append(args, i.ID)
	if _, err := s.db.Exec(`UPDATE issues SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
		return nil, err
	}
	s.issueChanged(p.ID)
	s.workAvailable()
	return s.issueByID(i.ID)
}

func (s *Server) listIssues(pid int64, status, query string) ([]*Issue, error) {
	q := `SELECT ` + issueCols + ` FROM issues WHERE project_id=?`
	args := []any{pid}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	if query != "" {
		q += ` AND (title LIKE ? OR body LIKE ? OR labels LIKE ?)`
		l := "%" + query + "%"
		args = append(args, l, l, l)
	}
	q += ` ORDER BY sort, number`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Issue{}
	for rows.Next() {
		if i, err := scanIssue(rows); err == nil {
			out = append(out, i)
		}
	}
	return out, nil
}

func (s *Server) loadIssueDetail(i *Issue) {
	i.Comments = []Comment{}
	rows, err := s.db.Query(`SELECT id,author,body,created_at FROM comments WHERE issue_id=? ORDER BY id`, i.ID)
	if err == nil {
		for rows.Next() {
			var c Comment
			rows.Scan(&c.ID, &c.Author, &c.Body, &c.CreatedAt)
			i.Comments = append(i.Comments, c)
		}
		rows.Close()
	}
	i.Runs = []Run{}
	rows, err = s.db.Query(runSelect+`WHERE r.issue_id=? ORDER BY r.started_at DESC`, i.ID)
	if err == nil {
		for rows.Next() {
			if r, err := scanRun(rows); err == nil {
				r.Prompt = ""
				i.Runs = append(i.Runs, *r)
			}
		}
		rows.Close()
	}
}
