// Package proto holds the wire types shared by the server and the runner.
package proto

import "encoding/json"

const Version = "0.1.0"

type GPU struct {
	Vendor   string `json:"vendor"`
	Name     string `json:"name"`
	MemoryMB int    `json:"memory_mb"`
}

type Hardware struct {
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	CPUModel   string   `json:"cpu_model"`
	CPUCores   int      `json:"cpu_cores"`
	MemoryGB   float64  `json:"memory_gb"`
	DiskFreeGB float64  `json:"disk_free_gb"`
	GPUs       []GPU    `json:"gpus"`
	Tools      []string `json:"tools"`
}

type RegisterRequest struct {
	NodeID         string   `json:"node_id"`
	Name           string   `json:"name"`
	Hardware       Hardware `json:"hardware"`
	Capabilities   []string `json:"capabilities"`
	Roles          []string `json:"roles"`
	MaxConcurrency int      `json:"max_concurrency"`
	ClaudePath     string   `json:"claude_path"`
	ClaudeVersion  string   `json:"claude_version"`
	ClaudeAuth     string   `json:"claude_auth"`
	RunnerVersion  string   `json:"runner_version"`
}

type RegisterResponse struct {
	NodeID string `json:"node_id"`
}

type PollRequest struct {
	NodeID     string   `json:"node_id"`
	FreeSlots  int      `json:"free_slots"`
	ActiveRuns []string `json:"active_runs"`
}

// Command is something the server asks a runner to do.
type Command struct {
	Type       string      `json:"type"` // assign | message | cancel
	Assignment *Assignment `json:"assignment,omitempty"`
	RunID      string      `json:"run_id,omitempty"`
	Message    string      `json:"message,omitempty"`
}

type PollResponse struct {
	Commands []Command `json:"commands"`
}

// Assignment fully describes one Claude Code run. Prompts are built by the
// server so that runners stay dumb and upgrades happen in one place.
type Assignment struct {
	RunID         string `json:"run_id"`
	Kind          string `json:"kind"` // work | review | chat
	ProjectSlug   string `json:"project_slug"`
	RepoURL       string `json:"repo_url"`
	Branch        string `json:"branch"`      // branch to check out / push
	BaseBranch    string `json:"base_branch"` // branch to create Branch from if missing
	Prompt        string `json:"prompt"`
	SystemPrompt  string `json:"system_prompt"`
	ResumeSession string `json:"resume_session,omitempty"`
	Model         string `json:"model,omitempty"`
}

type RunEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type RunFinish struct {
	Status    string  `json:"status"` // succeeded | failed | cancelled
	Error     string  `json:"error,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	Result    string  `json:"result,omitempty"`
	Pushed    bool    `json:"pushed"`
	Head      string  `json:"head,omitempty"`
}

// AgentCall is an MCP tool call forwarded from a Claude session to the server.
type AgentCall struct {
	RunID string          `json:"run_id"`
	Tool  string          `json:"tool"`
	Args  json.RawMessage `json:"args"`
}

type AgentResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// Tool is an MCP tool definition exposed to Claude.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	str  = map[string]any{"type": "string"}
	num  = map[string]any{"type": "integer"}
	strs = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	nums = map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}
)

func desc(base map[string]any, d string) map[string]any {
	m := map[string]any{"description": d}
	for k, v := range base {
		m[k] = v
	}
	return m
}

// Tools is the coordination toolset every agent gets.
var Tools = []Tool{
	{"list_issues", "List issues of the current project. Optional filter by status (backlog,todo,in_progress,review,done,blocked) or text query.",
		obj(map[string]any{"status": str, "query": str})},
	{"get_issue", "Get one issue with its full description, comments and history.",
		obj(map[string]any{"number": num}, "number")},
	{"create_issue", "Create a new issue on the shared board so that some agent (or a human) picks it up. Use for follow-up work you discover, or to split large work into parallel pieces. 'requires' lists capabilities a machine must have (e.g. gpu, cuda, docker). 'depends_on' lists issue numbers that must be done first.",
		obj(map[string]any{
			"title":      str,
			"body":       desc(str, "Markdown description with enough context for another agent to work on it independently"),
			"priority":   desc(str, "urgent | high | medium | low"),
			"labels":     strs,
			"requires":   strs,
			"depends_on": nums,
			"status":     desc(str, "todo (default, immediately claimable) or backlog"),
		}, "title")},
	{"update_issue", "Update fields of an issue (title, body, priority, labels, requires, depends_on, status).",
		obj(map[string]any{"number": num, "title": str, "body": str, "priority": str, "labels": strs, "requires": strs, "depends_on": nums, "status": str}, "number")},
	{"comment_issue", "Add a comment to an issue (progress notes, findings, questions for humans).",
		obj(map[string]any{"number": num, "body": str}, "number", "body")},
	{"complete_work", "Call when your assigned issue is implemented, committed and pushed to your branch. Hands it over to review.",
		obj(map[string]any{"summary": desc(str, "What you changed and how it was verified")}, "summary")},
	{"block_issue", "Call when you cannot proceed with the assigned issue and need a human (missing info, credentials, design decision).",
		obj(map[string]any{"reason": str}, "reason")},
	{"review_verdict", "Reviewers only: approve (branch gets merged into the base branch) or request changes (issue goes back to the queue with your comment).",
		obj(map[string]any{"verdict": desc(str, "approve | request_changes"), "comment": str}, "verdict", "comment")},
	{"post_chat", "Post a message to the project chat that humans and other agents see. 'to' may be a machine name to direct it at that machine's agents; omit to broadcast.",
		obj(map[string]any{"message": str, "to": str}, "message")},
	{"read_chat", "Read recent project chat messages (instructions from humans, notes from other agents).",
		obj(map[string]any{"limit": num})},
	{"list_agents", "List machines in the swarm with their hardware, capabilities, roles and what they are working on right now.",
		obj(map[string]any{})},
}
