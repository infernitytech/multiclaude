package server

import (
	"database/sql"
	"encoding/json"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS projects(
  id INTEGER PRIMARY KEY,
  slug TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  default_branch TEXT NOT NULL DEFAULT 'main',
  review_mode TEXT NOT NULL DEFAULT 'agent',
  instructions TEXT NOT NULL DEFAULT '',
  dispatch INTEGER NOT NULL DEFAULT 1,
  next_number INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS issues(
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL,
  number INTEGER NOT NULL,
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'todo',
  priority INTEGER NOT NULL DEFAULT 2,
  labels TEXT NOT NULL DEFAULT '[]',
  requires TEXT NOT NULL DEFAULT '[]',
  depends_on TEXT NOT NULL DEFAULT '[]',
  assignee TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  branch TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT 'human',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  sort REAL NOT NULL DEFAULT 0,
  UNIQUE(project_id, number)
);
CREATE INDEX IF NOT EXISTS issues_status ON issues(project_id, status);
CREATE TABLE IF NOT EXISTS comments(
  id INTEGER PRIMARY KEY,
  issue_id INTEGER NOT NULL,
  author TEXT NOT NULL,
  body TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS comments_issue ON comments(issue_id);
CREATE TABLE IF NOT EXISTS nodes(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  hardware TEXT NOT NULL DEFAULT '{}',
  caps TEXT NOT NULL DEFAULT '[]',
  roles TEXT NOT NULL DEFAULT '[]',
  max_conc INTEGER NOT NULL DEFAULT 1,
  extra_caps TEXT NOT NULL DEFAULT '[]',
  override_roles TEXT NOT NULL DEFAULT '',
  override_conc INTEGER NOT NULL DEFAULT 0,
  paused INTEGER NOT NULL DEFAULT 0,
  claude_path TEXT NOT NULL DEFAULT '',
  claude_version TEXT NOT NULL DEFAULT '',
  claude_auth TEXT NOT NULL DEFAULT '',
  runner_version TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  last_seen INTEGER NOT NULL DEFAULT 0,
  registered_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS runs(
  id TEXT PRIMARY KEY,
  project_id INTEGER NOT NULL,
  issue_id INTEGER,
  node_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  prompt TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  ended_at INTEGER NOT NULL DEFAULT 0,
  session_id TEXT NOT NULL DEFAULT '',
  cost_usd REAL NOT NULL DEFAULT 0,
  result TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  completed INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_node ON runs(node_id, status);
CREATE INDEX IF NOT EXISTS runs_issue ON runs(issue_id);
CREATE TABLE IF NOT EXISTS run_events(
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  type TEXT NOT NULL,
  data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS run_events_run ON run_events(run_id, id);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL,
  author TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_project ON messages(project_id, id);
CREATE TABLE IF NOT EXISTS node_projects(
  node_id TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  PRIMARY KEY(node_id, project_id)
);
CREATE TABLE IF NOT EXISTS chat_sessions(
  node_id TEXT NOT NULL,
  project_id INTEGER NOT NULL,
  session_id TEXT NOT NULL,
  PRIMARY KEY(node_id, project_id)
);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and makes claim transactions trivially atomic.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}

func now() int64 { return time.Now().UnixMilli() }

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func parseStrs(s string) []string {
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

func parseInts(s string) []int {
	var out []int
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []int{}
	}
	return out
}

// ---- model types -------------------------------------------------------

type Project struct {
	ID            int64          `json:"id"`
	Slug          string         `json:"slug"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	DefaultBranch string         `json:"default_branch"`
	ReviewMode    string         `json:"review_mode"` // agent | human | none
	Instructions  string         `json:"instructions"`
	Dispatch      bool           `json:"dispatch"`
	CreatedAt     int64          `json:"created_at"`
	Counts        map[string]int `json:"counts"`
}

const projectCols = `id,slug,name,description,default_branch,review_mode,instructions,dispatch,created_at`

func scanProject(r interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	err := r.Scan(&p.ID, &p.Slug, &p.Name, &p.Description, &p.DefaultBranch, &p.ReviewMode, &p.Instructions, &p.Dispatch, &p.CreatedAt)
	return p, err
}

type Issue struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"project_id"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	Priority  int       `json:"priority"`
	Labels    []string  `json:"labels"`
	Requires  []string  `json:"requires"`
	DependsOn []int     `json:"depends_on"`
	Assignee  string    `json:"assignee"`
	RunID     string    `json:"run_id"`
	Attempts  int       `json:"attempts"`
	Branch    string    `json:"branch"`
	CreatedBy string    `json:"created_by"`
	CreatedAt int64     `json:"created_at"`
	UpdatedAt int64     `json:"updated_at"`
	Sort      float64   `json:"sort"`
	Comments  []Comment `json:"comments"`
	Runs      []Run     `json:"runs"`
}

const issueCols = `id,project_id,number,title,body,status,priority,labels,requires,depends_on,assignee,run_id,attempts,branch,created_by,created_at,updated_at,sort`

func scanIssue(r interface{ Scan(...any) error }) (*Issue, error) {
	i := &Issue{}
	var labels, req, deps string
	err := r.Scan(&i.ID, &i.ProjectID, &i.Number, &i.Title, &i.Body, &i.Status, &i.Priority, &labels, &req, &deps,
		&i.Assignee, &i.RunID, &i.Attempts, &i.Branch, &i.CreatedBy, &i.CreatedAt, &i.UpdatedAt, &i.Sort)
	i.Labels, i.Requires, i.DependsOn = parseStrs(labels), parseStrs(req), parseInts(deps)
	return i, err
}

type Comment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

type Node struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Hardware      json.RawMessage `json:"hardware"`
	Caps          []string        `json:"caps"`  // reported by runner
	Roles         []string        `json:"roles"` // effective
	ReportedRoles []string        `json:"reported_roles"`
	MaxConc       int             `json:"max_conc"` // effective
	ReportedConc  int             `json:"reported_conc"`
	ExtraCaps     []string        `json:"extra_caps"` // added in UI
	OverrideRoles string          `json:"-"`
	OverrideConc  int             `json:"override_conc"`
	Paused        bool            `json:"paused"`
	ClaudePath    string          `json:"claude_path"`
	ClaudeVersion string          `json:"claude_version"`
	ClaudeAuth    string          `json:"claude_auth"`
	RunnerVersion string          `json:"runner_version"`
	Model         string          `json:"model"`
	LastSeen      int64           `json:"last_seen"`
	RegisteredAt  int64           `json:"registered_at"`
	Online        bool            `json:"online"`
	Active        []Run           `json:"active"`
	Projects      []int64         `json:"projects"` // assigned project ids
}

func (n *Node) AllCaps() []string { return append(append([]string{}, n.Caps...), n.ExtraCaps...) }

func (n *Node) HasRole(r string) bool {
	for _, x := range n.Roles {
		if x == r {
			return true
		}
	}
	return false
}

const nodeCols = `id,name,hardware,caps,roles,max_conc,extra_caps,override_roles,override_conc,paused,claude_path,claude_version,claude_auth,runner_version,model,last_seen,registered_at`

func scanNode(r interface{ Scan(...any) error }) (*Node, error) {
	n := &Node{}
	var hw, caps, roles, extra string
	err := r.Scan(&n.ID, &n.Name, &hw, &caps, &roles, &n.ReportedConc, &extra, &n.OverrideRoles, &n.OverrideConc, &n.Paused,
		&n.ClaudePath, &n.ClaudeVersion, &n.ClaudeAuth, &n.RunnerVersion, &n.Model, &n.LastSeen, &n.RegisteredAt)
	n.Hardware = json.RawMessage(hw)
	n.Caps, n.ReportedRoles, n.ExtraCaps = parseStrs(caps), parseStrs(roles), parseStrs(extra)
	n.Roles = n.ReportedRoles
	if n.OverrideRoles != "" {
		n.Roles = parseStrs(n.OverrideRoles)
	}
	n.MaxConc = n.ReportedConc
	if n.OverrideConc > 0 {
		n.MaxConc = n.OverrideConc
	}
	n.Online = now()-n.LastSeen < 70_000
	return n, err
}

type Run struct {
	ID          string  `json:"id"`
	ProjectID   int64   `json:"project_id"`
	IssueID     *int64  `json:"issue_id"`
	NodeID      string  `json:"node_id"`
	Kind        string  `json:"kind"`
	Status      string  `json:"status"`
	Prompt      string  `json:"prompt,omitempty"`
	StartedAt   int64   `json:"started_at"`
	EndedAt     int64   `json:"ended_at"`
	SessionID   string  `json:"session_id"`
	CostUSD     float64 `json:"cost_usd"`
	Result      string  `json:"result"`
	Error       string  `json:"error"`
	Completed   bool    `json:"completed"`
	Verdict     string  `json:"verdict"`
	NodeName    string  `json:"node_name,omitempty"`
	IssueNumber int     `json:"issue_number,omitempty"`
	IssueTitle  string  `json:"issue_title,omitempty"`
	ProjectSlug string  `json:"project_slug,omitempty"`
}

const runSelect = `SELECT r.id,r.project_id,r.issue_id,r.node_id,r.kind,r.status,r.prompt,r.started_at,r.ended_at,r.session_id,r.cost_usd,r.result,r.error,r.completed,r.verdict,
 COALESCE(n.name,''),COALESCE(i.number,0),COALESCE(i.title,''),COALESCE(p.slug,'')
 FROM runs r LEFT JOIN nodes n ON n.id=r.node_id LEFT JOIN issues i ON i.id=r.issue_id LEFT JOIN projects p ON p.id=r.project_id `

func scanRun(r interface{ Scan(...any) error }) (*Run, error) {
	x := &Run{}
	err := r.Scan(&x.ID, &x.ProjectID, &x.IssueID, &x.NodeID, &x.Kind, &x.Status, &x.Prompt, &x.StartedAt, &x.EndedAt, &x.SessionID,
		&x.CostUSD, &x.Result, &x.Error, &x.Completed, &x.Verdict, &x.NodeName, &x.IssueNumber, &x.IssueTitle, &x.ProjectSlug)
	return x, err
}

type Message struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Author    string `json:"author"`
	Target    string `json:"target"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

var priorityNames = []string{"urgent", "high", "medium", "low"}

func priorityFromName(s string) (int, bool) {
	for i, n := range priorityNames {
		if n == s {
			return i, true
		}
	}
	return 2, false
}

var validStatus = map[string]bool{"backlog": true, "todo": true, "in_progress": true, "review": true, "done": true, "blocked": true}
