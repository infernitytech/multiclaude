package server

import (
	"fmt"
	"strings"
	"sync"

	"multiclaude/internal/proto"
)

// Agent-to-agent communication. Every running session (run) is an "agent"
// addressable by its handle "<machine>/<short run id>", so several agents on
// the same machine can still be told apart.

const agentMsgLimit = 40 // max agent-to-agent messages a single run may send (loop guard)

var (
	agentMsgMu    sync.Mutex
	agentMsgCount = map[string]int{}
)

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

func agentHandle(nodeName, runID string) string { return nodeName + "/" + shortID(runID) }

func (r *Run) handle() string { return agentHandle(r.NodeName, r.ID) }

// activity describes what an agent is doing, e.g. `working on #12 "Add login"`.
func (r *Run) activity() string {
	switch {
	case r.Kind == "work" && r.IssueNumber > 0:
		return fmt.Sprintf("working on #%d %q", r.IssueNumber, r.IssueTitle)
	case r.Kind == "review" && r.IssueNumber > 0:
		return fmt.Sprintf("reviewing #%d %q", r.IssueNumber, r.IssueTitle)
	case r.Kind == "chat":
		return "chat session (talking with humans / planning)"
	}
	return r.Kind
}

// projectAgents returns the running sessions of a project.
func (s *Server) projectAgents(pid int64) []*Run {
	rows, err := s.db.Query(runSelect+`WHERE r.project_id=? AND r.status='running' ORDER BY n.name, r.started_at`, pid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		if r, err := scanRun(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// findAgent resolves "<machine>/<id prefix>" (or a bare run id prefix) to a running session of the project.
func (s *Server) findAgent(pid int64, ref string) (*Run, bool) {
	ref = strings.TrimSpace(ref)
	machine, id := "", ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		machine, id = ref[:i], ref[i+1:]
	}
	if id == "" {
		return nil, false
	}
	var match *Run
	for _, r := range s.projectAgents(pid) {
		if strings.HasPrefix(r.ID, id) && (machine == "" || strings.EqualFold(machine, r.NodeName)) {
			if match != nil {
				return nil, false // ambiguous
			}
			match = r
		}
	}
	return match, match != nil
}

// agentRoster lists the running agents of a project for prompts and list_agents.
func (s *Server) agentRoster(pid int64, selfRunID string) string {
	var b strings.Builder
	for _, r := range s.projectAgents(pid) {
		you := ""
		if r.ID == selfRunID {
			you = " (you)"
		}
		fmt.Fprintf(&b, "  • %s%s — %s\n", r.handle(), you, r.activity())
	}
	return b.String()
}

// sendToAgents injects a message from one agent into other running sessions.
// It never starts new sessions, so agents cannot fan out work to idle
// machines by chatting (they create issues for that).
func (s *Server) sendToAgents(from *Run, targets []*Run, body string) []string {
	msg := fmt.Sprintf("[Message from agent %s, who is %s]\n%s\n\n(To answer, call post_chat with to=%q. Do not reply to plain acknowledgements; keep working on your own task.)",
		from.handle(), from.activity(), body, from.handle())
	var delivered []string
	for _, t := range targets {
		if t.ID == from.ID {
			continue
		}
		s.enqueue(t.NodeID, proto.Command{Type: "message", RunID: t.ID, Message: msg})
		delivered = append(delivered, t.handle())
	}
	return delivered
}

func takeAgentMsgBudget(runID string) bool {
	agentMsgMu.Lock()
	defer agentMsgMu.Unlock()
	if agentMsgCount[runID] >= agentMsgLimit {
		return false
	}
	agentMsgCount[runID]++
	return true
}

func forgetAgentMsgBudget(runID string) {
	agentMsgMu.Lock()
	delete(agentMsgCount, runID)
	agentMsgMu.Unlock()
}

// postChatFromAgent implements the post_chat tool.
func (s *Server) postChatFromAgent(p *Project, run *Run, to, msg string) (string, error) {
	if strings.TrimSpace(msg) == "" {
		return "", fmt.Errorf("message is empty")
	}
	to = strings.TrimSpace(to)
	if to != "" && to != "all" && !takeAgentMsgBudget(run.ID) {
		return "", fmt.Errorf("agent message limit (%d) reached for this session; finish your task or use comment_issue / the board instead", agentMsgLimit)
	}
	me := run.NodeName
	switch {
	case to == "" || to == "all" || to == "*":
		s.postMessage(p.ID, me, "", msg)
		var others []*Run
		for _, r := range s.projectAgents(p.ID) {
			if r.ID != run.ID {
				others = append(others, r)
			}
		}
		if len(others) == 0 {
			return "Posted to the project chat. No other agents are running on this project right now.", nil
		}
		if !takeAgentMsgBudget(run.ID) {
			return "Posted to the project chat (not delivered live: agent message limit reached for this session).", nil
		}
		d := s.sendToAgents(run, others, msg)
		return "Posted to the project chat and delivered live to: " + strings.Join(d, ", "), nil

	case strings.Contains(to, "/"):
		t, ok := s.findAgent(p.ID, to)
		if !ok {
			return "", fmt.Errorf("no running agent %q on this project (see list_agents; it may have finished — then use comment_issue or create_issue)", to)
		}
		if t.ID == run.ID {
			return "", fmt.Errorf("that is you")
		}
		s.postMessage(p.ID, me, t.NodeID, "@"+t.handle()+" "+msg)
		s.sendToAgents(run, []*Run{t}, msg)
		return "Delivered to " + t.handle() + ".", nil

	default:
		n, err := s.nodeByName(to)
		if err != nil {
			if t, ok := s.findAgent(p.ID, to); ok { // bare run id
				return s.postChatFromAgent(p, run, t.handle(), msg)
			}
			return "", fmt.Errorf("unknown machine or agent %q (see list_agents)", to)
		}
		s.postMessage(p.ID, me, n.ID, msg)
		var targets []*Run
		for _, r := range s.projectAgents(p.ID) {
			if r.NodeID == n.ID && r.ID != run.ID {
				targets = append(targets, r)
			}
		}
		if len(targets) > 0 {
			return "Delivered to " + strings.Join(s.sendToAgents(run, targets, msg), ", ") + ".", nil
		}
		if n.ID == run.NodeID {
			return "Posted. No other agents of this project are running on your machine.", nil
		}
		// Explicitly addressed idle machine: start a chat session there.
		return "Posted. " + s.deliverChat(p, []*Node{n}, agentHandle(run.NodeName, run.ID), msg), nil
	}
}
