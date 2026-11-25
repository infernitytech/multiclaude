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

func (s *Server) systemPrompt(p *Project, n *Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are an autonomous Claude Code agent on machine %q (%s; capabilities: %s; roles: %s).
You are part of MultiClaude: a swarm of Claude Code agents on several machines working on the same git repository, coordinated by a central server with an issue board. Humans supervise through the board and the project chat.

Coordination rules:
- The "multiclaude" MCP tools are your interface to the board and chat: list_issues, get_issue, create_issue, update_issue, comment_issue, complete_work, block_issue, review_verdict, post_chat, read_chat, list_agents.
- Stay within the scope of your assignment. Record other work you discover as new issues (create_issue) with enough context for another agent; set "requires" (e.g. ["gpu"]) when only specific hardware can do it, and "depends_on" for ordering.
- Large work: split it into independent issues so several machines can work in parallel, and/or use subagents (Task tool) inside your own assignment.
- Other agents push to the same repository concurrently. Only push your own branch unless told otherwise. Never force-push the default branch %q.
- Human instructions arriving in this conversation (marked as project chat messages) take priority. Answer them briefly and continue.
- Git LFS is available on the server; track large binary files with git lfs.
`, n.Name, hwSummary(n), strings.Join(n.AllCaps(), ", "), strings.Join(n.Roles, ", "), p.DefaultBranch)
	if strings.TrimSpace(p.Instructions) != "" {
		fmt.Fprintf(&b, "\nProject instructions from the humans:\n%s\n", p.Instructions)
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
		SystemPrompt: s.systemPrompt(p, n),
		Model:        n.Model,
	}
	if i != nil {
		a.Branch = i.Branch
	}
	switch kind {
	case "work":
		a.Prompt = fmt.Sprintf(`Your assignment:

%s
You are in a git checkout of the project on branch %q (created from %q if it did not exist yet; it may contain earlier work on this issue).

Steps:
1. Check read_chat briefly for relevant human instructions. Understand the codebase parts relevant to the issue.
2. Implement the issue. Build/test/verify as far as possible on this machine.
3. Commit with messages referencing #%d. If origin/%s moved on, merge it into your branch and resolve conflicts.
4. Push: git push -u origin %s
5. Call complete_work with a summary of what changed and how it was verified.
If you are truly blocked (needs a human decision, credentials, unavailable hardware), call block_issue with the reason instead.`,
			s.issueContext(i), a.Branch, p.DefaultBranch, i.Number, p.DefaultBranch, a.Branch)
	case "review":
		a.Prompt = fmt.Sprintf(`You are the reviewer for:

%s
The implementation is on branch %q, which is checked out. The base branch is %q.

Steps:
1. Inspect the change: git log origin/%s..HEAD and git diff origin/%s...HEAD
2. Build and run tests where possible. Check correctness, completeness against the issue, scope, code quality and that nothing unrelated broke.
3. Trivial fixes you may commit and push to %q yourself. Make sure the branch merges cleanly with origin/%s (merge it in and push if needed).
4. Finish by calling review_verdict:
   - "approve" → the server merges the branch into %s.
   - "request_changes" with concrete, actionable feedback → the issue goes back to the queue for a worker.`,
			s.issueContext(i), a.Branch, p.DefaultBranch, p.DefaultBranch, p.DefaultBranch, a.Branch, p.DefaultBranch, p.DefaultBranch)
	case "chat":
		a.Prompt = prompt
	}
	return a
}

func chatPrompt(author, text string) string {
	return fmt.Sprintf(`[Project chat message from %s]
%s

(You are in a checkout of the project's default branch. You can answer questions about the code, plan work and manage the board with the multiclaude tools — e.g. break a request into issues with create_issue so the swarm works on them in parallel. If you are explicitly asked to change code directly, commit and push to the default branch (git pull --rebase first). Your final answer is posted to the project chat, so keep it concise.)`, author, text)
}
