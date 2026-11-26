package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"multiclaude/internal/proto"
)

func hwSummary(n *Node) string {
	var hw proto.Hardware
	json.Unmarshal(n.Hardware, &hw)
	parts := []string{fmt.Sprintf("%s/%s", hw.OS, hw.Arch)}
	if hw.CPUCores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", hw.CPUCores))
	}
	if hw.MemoryGB > 0 {
		parts = append(parts, fmt.Sprintf("%.0f GB RAM", hw.MemoryGB))
	}
	for _, g := range hw.GPUs {
		parts = append(parts, fmt.Sprintf("GPU %s (%d MB)", g.Name, g.MemoryMB))
	}
	return strings.Join(parts, ", ")
}

func (s *Server) systemPrompt(runID string, p *Project, n *Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are an autonomous Claude Code agent on machine %q (%s; capabilities: %s; roles: %s). Your agent id is %q.
You are part of MultiClaude: a swarm of Claude Code agents on several machines working on the same git repository, coordinated by a central server with an issue board. Humans supervise through the board and the project chat.

Coordination rules:
- The "multiclaude" MCP tools are your interface to the board and chat: list_issues, get_issue, create_issue, update_issue, comment_issue, complete_work, block_issue, review_verdict, post_chat, read_chat, list_agents.
- Stay within the scope of your assignment. Record other work you discover as new issues (create_issue) with enough context for another agent; set "requires" only when specific hardware is needed (see "Hardware routing" below), and "depends_on" for ordering.
- Large work: split it into independent issues so several machines can work in parallel, and/or use subagents (Task tool) inside your own assignment.
- Other agents push to the same repository concurrently. Only push your own branch unless told otherwise. Never force-push the default branch %q.
- Human instructions arriving in this conversation (marked as project chat messages) take priority. Answer them briefly and continue.
- Other agents work on this project at the same time. Coordinate with them directly instead of duplicating or colliding with their work: post_chat with to="<agent id>" (one agent, e.g. "machine/abc123"), to="<machine>" (that machine's agents) or without "to" (every running agent + humans). list_agents shows who is running and what they do. Use it for interface agreements, heads-ups about shared files, questions and handoffs. Messages from agents arrive in this conversation marked "[Message from agent …]"; answer with post_chat when an answer is needed, keep it short, and don't reply to acknowledgements. Agents that have finished cannot receive messages — use comment_issue / create_issue for durable notes.
- Git LFS is available on the server; track large binary files with git lfs.

How the swarm works (so you know what happens around you):
- Sessions: every agent is one Claude Code session for exactly one job — one issue to implement ("work"), one issue to review ("review"), or a "chat" session answering the humans. Nobody loops: when your job is done you end your turn, and ending your turn ends your session. The machine's runner then frees the slot and claims the next job.
- Issue lifecycle: backlog → todo → in_progress (a worker claimed it) → review → done (merged into %[6]q). "blocked" means a human must act. Only "todo" issues are picked up, and only once all their depends_on issues are done; "backlog" is parked.
- Claiming is done by the server, never by agents. Each runner long-polls the server; when a machine has a free slot the server picks, in order: reviews first (they unblock merges), then issues whose "requires" need this machine's special capabilities, then priority (urgent > high > medium > low), then board order. A machine only gets issues of projects it is assigned to, whose "requires" are a subset of its capabilities, matching its roles (worker / reviewer). The claim is atomic, so no two agents ever hold the same issue.
- Consequences for you: never try to claim or start other issues yourself and don't move other issues to in_progress/review/done — create well-described todo issues (with priority, requires, depends_on) and the swarm picks them up in parallel. Writing a good issue is how you delegate.
- Branches: each issue is worked on its own branch issue-<number> pushed to origin; the server merges it into %[6]q after approval (or directly in "no review" mode). If a session ends without its finish signal, the attempt counts as failed: the issue goes back to todo with your final output as a comment (uncommitted changes are salvaged as a WIP commit on the branch); after 3 failed attempts it becomes blocked.
`, n.Name, hwSummary(n), strings.Join(n.AllCaps(), ", "), strings.Join(n.Roles, ", "), agentHandle(n.Name, runID), p.DefaultBranch)
	b.WriteString(s.projectContext(p))
	if roster := s.agentRoster(p.ID, runID); roster != "" {
		fmt.Fprintf(&b, "\nAgents running on this project when you started (may change; use list_agents):\n%s", roster)
	} else {
		b.WriteString("\nNo other agents were running on this project when you started (use list_agents to check later).\n")
	}
	b.WriteString(s.hardwareRouting(p))
	b.WriteString(s.briefing(p))
	if strings.TrimSpace(p.Instructions) != "" {
		fmt.Fprintf(&b, "\nProject instructions from the humans:\n%s\n", p.Instructions)
	}
	return b.String()
}

var reviewModeText = map[string]string{
	"agent": "agent review — after complete_work a reviewer agent reviews your branch and the server merges it on approval",
	"human": "human review — after complete_work a human reviews and merges your branch",
	"none":  "no review — the server merges your branch into the default branch when you call complete_work",
}

// projectContext describes the project itself. Facts only the runner knows
// (exact clone URL, local checkout path) are appended by the runner.
func (s *Server) projectContext(p *Project) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nProject:\n- Name: %s\n- Slug: %s (git repository %s.git on the MultiClaude server; it is the \"origin\" remote of your checkout)\n- Default branch: %s\n",
		p.Name, p.Slug, p.Slug, p.DefaultBranch)
	if rm, ok := reviewModeText[p.ReviewMode]; ok {
		fmt.Fprintf(&b, "- Review mode: %s\n", rm)
	}
	if d := strings.TrimSpace(p.Description); d != "" {
		fmt.Fprintf(&b, "- Description:\n%s\n", d)
	}
	return b.String()
}

func (s *Server) issueContext(i *Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue #%d: %s\nPriority: %s", i.Number, i.Title, priorityNames[clampPrio(i.Priority)])
	if len(i.Labels) > 0 {
		fmt.Fprintf(&b, " | Labels: %s", strings.Join(i.Labels, ", "))
	}
	if len(i.Requires) > 0 {
		fmt.Fprintf(&b, " | Requires: %s", strings.Join(i.Requires, ", "))
	}
	fmt.Fprintf(&b, "\n\n%s\n", strings.TrimSpace(i.Body))
	rows, err := s.db.Query(`SELECT author, body FROM comments WHERE issue_id=? ORDER BY id`, i.ID)
	if err == nil {
		first := true
		for rows.Next() {
			var a, c string
			rows.Scan(&a, &c)
			if strings.HasPrefix(c, "🤖 started") {
				continue
			}
			if first {
				b.WriteString("\nComments / history:\n")
				first = false
			}
			fmt.Fprintf(&b, "--- %s:\n%s\n", a, c)
		}
		rows.Close()
	}
	return b.String()
}

func clampPrio(p int) int {
	if p < 0 || p > 3 {
		return 2
	}
	return p
}

func (s *Server) buildAssignment(runID, kind string, p *Project, i *Issue, n *Node, prompt string) *proto.Assignment {
	a := &proto.Assignment{
		RunID:        runID,
		Kind:         kind,
		ProjectSlug:  p.Slug,
		RepoURL:      "/git/" + p.Slug + ".git",
		BaseBranch:   p.DefaultBranch,
		Branch:       p.DefaultBranch,
		SystemPrompt: s.systemPrompt(runID, p, n),
		Model:        n.Model,
	}
	if i != nil {
		a.Branch = i.Branch
	}
	switch kind {
	case "work":
		a.Prompt = fmt.Sprintf(`Your assignment (a "work" session):

%[1]s
You are in a git checkout of the project on branch %[2]q (created from %[3]q if it did not exist yet). If earlier attempts or review feedback exist, they are in the comments above and their commits may already be on this branch — continue from there instead of starting over, and address every point of review feedback.

Rules for this session:
- Exactly one task: issue #%[4]d. Do not work on other issues, even small ones. Out-of-scope work you discover (bugs, follow-ups, refactors, missing pieces) goes into new issues via create_issue with enough context that another agent can do it without you.
- Too big for one session? Do a coherent, working first part, and create follow-up issues for the rest (with depends_on where order matters). Use subagents (Task tool) inside your session for parallelizable sub-work.
- Work autonomously. Humans may look in and send you messages, but nobody answers questions in real time, and ending your turn ends the session. Never stop to ask a question or present options and wait — decide sensibly yourself and state the assumption in your summary. Only a genuine blocker (a human decision that changes the result, missing credentials/access, unavailable hardware) justifies block_issue.
- Other agents may be changing related code right now (see list_agents). Before changing shared interfaces or files another agent is clearly working on, tell them with post_chat.

Steps:
1. Check read_chat briefly for relevant human instructions. Read the parts of the codebase relevant to the issue.
2. Implement the issue. Build, run tests and verify as far as possible on this machine.
3. Commit with messages referencing #%[4]d. If origin/%[3]s moved on, merge it into your branch and resolve conflicts.
4. Push: git push -u origin %[2]s
5. Finish signal — exactly one of:
   - complete_work with a summary: what changed, how it was verified, assumptions you made, follow-up issues you created. %[5]s
   - block_issue with a precise reason and what a human needs to provide.
   Then end your turn with a one-paragraph summary. Ending without either call counts as a failed attempt.`,
			s.issueContext(i), a.Branch, p.DefaultBranch, i.Number, afterComplete(p))
	case "review":
		a.Prompt = fmt.Sprintf(`Your assignment (a "review" session) — you are the reviewer for:

%[1]s
The implementation is on branch %[2]q, which is checked out. The base branch is %[3]q. The worker's summary is in the comments above ("Work completed").

Rules for this session:
- Exactly one task: decide whether issue #%[4]d's branch is good to merge. Don't implement missing features yourself — that is the worker's job; send it back with request_changes.
- Work autonomously; nobody answers questions in real time, and ending your turn ends the session.
- You may commit trivial fixes (typos, formatting, an obvious one-line bug, a merge with %[3]s) and push them to %[2]q.
- Unrelated problems you notice in the codebase go into new issues via create_issue, not into this verdict.

Steps:
1. Inspect the change: git log origin/%[3]s..HEAD and git diff origin/%[3]s...HEAD
2. Build and run tests where possible. Check: does it fully solve the issue, is it correct, does it stay in scope, is the code quality acceptable, did nothing unrelated break.
3. Make sure the branch merges cleanly with origin/%[3]s (merge it in and push if needed).
4. Finish signal — call review_verdict exactly once:
   - "approve" → when your session ends the server merges %[2]s into %[3]s and the issue is done.
   - "request_changes" with concrete, actionable feedback (what is wrong, where, what "done" looks like) → the issue goes back to todo and the next worker gets your comment.
   Then end your turn with a short summary. Ending without a verdict counts as a failed review attempt.`,
			s.issueContext(i), a.Branch, p.DefaultBranch, i.Number)
	case "chat":
		a.Prompt = prompt
	}
	return a
}

// afterComplete tells a worker what happens after complete_work in this project.
func afterComplete(p *Project) string {
	switch p.ReviewMode {
	case "none":
		return "The server then merges your branch into " + p.DefaultBranch + " directly (no review) — it must be complete and working."
	case "human":
		return "A human then reviews and merges your branch."
	}
	return "A reviewer agent then reviews your branch; on approval the server merges it into " + p.DefaultBranch + "."
}

func chatPrompt(author, text string) string {
	return fmt.Sprintf(`[Project chat message from %s]
%s

(This is a "chat" session in a checkout of the project's default branch. You can answer questions about the code, plan work and manage the board with the multiclaude tools. For real work, prefer breaking the request into well-described issues with create_issue (priority, requires, depends_on) so the swarm works on them in parallel — the server dispatches them automatically. Only if you are explicitly asked to change code directly, do it yourself: commit and push to the default branch (git pull --rebase first). Your final answer is posted to the project chat and ends this turn, so keep it concise.)`, author, text)
}
