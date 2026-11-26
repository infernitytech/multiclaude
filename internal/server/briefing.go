package server

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// projectMachines returns the machines assigned to a project.
func (s *Server) projectMachines(pid int64) []*Node {
	nodes, _ := s.listNodes()
	var out []*Node
	for _, n := range nodes {
		for _, id := range n.Projects {
			if id == pid {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// schedulabilityNote warns (for tool results) when no assigned machine can
// ever take the issue because of its "requires".
func (s *Server) schedulabilityNote(p *Project, i *Issue) string {
	if i == nil || len(i.Requires) == 0 || i.Status == "done" {
		return ""
	}
	var online, offline []string
	for _, n := range s.projectMachines(p.ID) {
		if n.HasRole("worker") && canEverRun(n, i.Requires) {
			if n.Online {
				online = append(online, n.Name)
			} else {
				offline = append(offline, n.Name)
			}
		}
	}
	switch {
	case len(online) > 0:
		return fmt.Sprintf(" Machines that can take it: %s.", strings.Join(online, ", "))
	case len(offline) > 0:
		return fmt.Sprintf(" Note: only offline machines (%s) have %s; it waits until one comes online.", strings.Join(offline, ", "), strings.Join(i.Requires, "+"))
	}
	return fmt.Sprintf(" ⚠ WARNING: no worker machine assigned to this project has all of [%s], so nobody will ever pick this issue up. Fix 'requires' (see the hardware routing list in your instructions) unless a human is going to add such a machine.", strings.Join(i.Requires, ", "))
}

// markNoMachine flags open issues whose "requires" no assigned worker can
// satisfy (with the real GPU rules: gpus:N, vram:N, architecture, …).
func (s *Server) markNoMachine(pid int64, issues []*Issue) {
	var workers []*Node
	for _, n := range s.projectMachines(pid) {
		if n.HasRole("worker") {
			workers = append(workers, n)
		}
	}
	for _, i := range issues {
		if len(i.Requires) == 0 || i.Status == "done" {
			continue
		}
		i.NoMachine = true
		for _, n := range workers {
			if canEverRun(n, i.Requires) {
				i.NoMachine = false
				break
			}
		}
	}
}

// hardwareRouting tells agents which capabilities exist and how to route work.
func (s *Server) hardwareRouting(p *Project) string {
	machines := s.projectMachines(p.ID)
	var b strings.Builder
	b.WriteString("\nHardware routing (the \"requires\" field of issues):\n")
	if len(machines) == 0 {
		b.WriteString("- No machines are assigned to this project yet.\n")
		return b.String()
	}
	capCount := map[string]int{}
	for _, n := range machines {
		state := "online"
		if !n.Online {
			state = "offline"
		}
		if n.Paused {
			state += ", paused"
		}
		fmt.Fprintf(&b, "- %s (%s; roles %s): %s\n  capabilities: %s\n", n.Name, state, strings.Join(n.Roles, "/"), hwSummary(n), strings.Join(n.AllCaps(), ", "))
		if len(n.DisabledCaps) > 0 {
			fmt.Fprintf(&b, "  switched off by the humans (reserved for something else — don't route work to them or use them there): %s\n", strings.Join(n.DisabledCaps, ", "))
		}
		if gpus := nodeGPUs(n); len(gpus) > 1 {
			var res []string
			for i := range gpus {
				if gpuReserved(n, i) {
					res = append(res, fmt.Sprintf("GPU %d", i))
				}
			}
			fmt.Fprintf(&b, "  %d GPUs → issues can ask for up to gpus:%d here", len(gpus), len(gpus)-len(res))
			if len(res) > 0 {
				fmt.Fprintf(&b, " (%s reserved by the humans)", strings.Join(res, ", "))
			}
			b.WriteString("\n")
		}
		for _, c := range n.AllCaps() {
			capCount[strings.ToLower(c)]++
		}
	}
	var special []string
	for c, k := range capCount {
		if k < len(machines) {
			special = append(special, c)
		}
	}
	sort.Strings(special)
	if len(special) > 0 {
		fmt.Fprintf(&b, "- Capabilities only some machines have (the ones worth routing on): %s\n", strings.Join(special, ", "))
	} else if len(machines) > 1 {
		b.WriteString("- All machines have the same capabilities, so \"requires\" is rarely needed.\n")
	}
	b.WriteString(`- Rules: an issue is only given to a machine that has ALL capabilities in its "requires". Leave "requires" empty when any machine can do the work — that keeps it parallel. Set it only for real needs: gpu/cuda/nvidia/rocm/mps for GPU work (training, CUDA kernels, GPU benchmarks), macos for Xcode/iOS/macOS builds, bigmem for very memory-hungry jobs, a tool name (docker, blender, unity, godot, ffmpeg, nvcc, …) when that tool must be installed. Use only capability names from the list above; anything else makes the issue unclaimable (create_issue/update_issue warn you).
- GPUs are handed out exclusively per session: "gpu" (or cuda/rocm/…) = one GPU, "gpus:N" = N GPUs on the same machine (multi-GPU training, DDP, big models split across GPUs), "vram:N" = every assigned GPU has at least N GB. Several GPU issues run in parallel on a multi-GPU machine, one per free GPU — so prefer several 1-GPU issues (e.g. a hyper-parameter sweep) over one big job when the work allows it, and ask for gpus:N only when one job really needs several GPUs at once. GPU generation: an architecture tag (e.g. "blackwell", "hopper", "ada", "ampere", "cdna3") or "sm_90" (exact compute capability) / "sm>=80" (at least, e.g. bf16 or FlashAttention needs) / "gfx942" (AMD target) restricts the issue to matching GPUs — use it when code is built for a specific architecture (CUDA arch flags, FP8/FP4 kernels, TensorRT engines), not by default.
- Split mixed work: CPU-only preparation (code, data pipeline, tests) as one issue, the hardware-specific run as another with "requires" and "depends_on". Keep hardware-bound issues small, since few machines can take them; machines with special capabilities prefer such issues.
- If you need hardware this machine doesn't have, don't fake it: create an issue with the right "requires" (or call block_issue if your own assignment can't be done here).
`)
	return b.String()
}

// briefing is a compact snapshot of the project's state for a new session.
// It is bounded in size no matter how large the board is.
func (s *Server) briefing(p *Project) string {
	var b strings.Builder
	b.WriteString("\nProject briefing (snapshot at session start; use list_issues / get_issue / read_chat for details):\n")

	counts := map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM issues WHERE project_id=? GROUP BY status`, p.ID)
	if err == nil {
		for rows.Next() {
			var st string
			var n int
			rows.Scan(&st, &n)
			counts[st] = n
		}
		rows.Close()
	}
	var parts []string
	for _, st := range []string{"backlog", "todo", "in_progress", "review", "blocked", "done"} {
		parts = append(parts, fmt.Sprintf("%s %d", st, counts[st]))
	}
	fmt.Fprintf(&b, "- Board: %s\n", strings.Join(parts, " · "))

	section := func(title string, q issueQuery, line func(*Issue) string) {
		items, total, err := s.queryIssues(p.ID, q)
		if err != nil || len(items) == 0 {
			return
		}
		more := ""
		if total > len(items) {
			more = fmt.Sprintf(" (%d of %d)", len(items), total)
		}
		fmt.Fprintf(&b, "- %s%s:\n", title, more)
		for _, i := range items {
			fmt.Fprintf(&b, "    #%d %s\n", i.Number, line(i))
		}
	}
	plain := func(i *Issue) string {
		x := i.Title
		if len(i.Requires) > 0 {
			x += " [requires " + strings.Join(i.Requires, "+") + "]"
		}
		return x
	}
	section("In progress / in review", issueQuery{Status: "in_progress,review", Order: "updated", Limit: 15}, func(i *Issue) string {
		return fmt.Sprintf("%s — %s by %s", i.Title, strings.ReplaceAll(i.Status, "_", " "), i.Assignee)
	})
	section("Next up (todo by priority)", issueQuery{Status: "todo", Order: "priority", Limit: 10}, func(i *Issue) string {
		return "(" + priorityNames[clampPrio(i.Priority)] + ") " + plain(i)
	})
	section("Blocked (needs a human)", issueQuery{Status: "blocked", Order: "updated", Limit: 8}, plain)
	section("Recently completed", issueQuery{Status: "done", Order: "updated", Limit: 10}, plain)

	repos := s.projectRepos(p)
	for i, rp := range repos {
		n := 8
		if i > 0 {
			n = 4
		}
		label := rp.DefaultBranch
		if len(repos) > 1 {
			label = rp.Name + " " + rp.DefaultBranch
		}
		if commits, err := s.gitLog(rp.Key, rp.DefaultBranch, n); err == nil && len(commits) > 0 {
			fmt.Fprintf(&b, "- Latest commits on %s:\n", label)
			for _, c := range commits {
				fmt.Fprintf(&b, "    %s %s (%s)\n", short(c.SHA), c.Subject, c.Author)
			}
		} else {
			fmt.Fprintf(&b, "- %s has no commits yet besides the initial one — you may be laying the foundation.\n", label)
		}
	}
	b.WriteString(`- Shared memory: read CLAUDE.md and README.md in the repo first if they exist; they hold the project's conventions, architecture and build/test commands. When you establish something future agents must know (layout, conventions, how to build/test/run), add it to CLAUDE.md as part of your change — concise and factual. Look at recently completed issues (get_issue) for decisions that were already made.
`)
	return b.String()
}

// markWaiting explains, for every queued issue (To do, or a review waiting for
// a reviewer agent), why no machine is working on it right now — the board
// shows it so a stalled queue is visible instead of silently idle.
func (s *Server) markWaiting(p *Project, issues []*Issue) {
	var machines []*Node
	running := map[string]int{}
	leases := map[string]map[int]string{}
	for _, i := range issues {
		if i.RunID != "" || (i.Status != "todo" && i.Status != "review") {
			continue
		}
		if machines == nil {
			machines = s.projectMachines(p.ID)
			for _, n := range machines {
				running[n.ID] = s.runningCount(n.ID)
				leases[n.ID] = s.gpuLeases(n.ID)
			}
		}
		i.Waiting = s.waitReason(p, i, machines, running, leases)
	}
}

func (s *Server) waitReason(p *Project, i *Issue, machines []*Node, running map[string]int, leases map[string]map[int]string) string {
	role, what := "worker", "worker"
	if i.Status == "review" {
		switch p.ReviewMode {
		case "human":
			return "waiting for a human review (review mode: human)"
		case "none":
			return ""
		}
		role, what = "reviewer", "reviewer"
	}
	if !p.Dispatch {
		return "work on this project is stopped (Resume to continue)"
	}
	if i.Status == "todo" && !s.depsDone(i) {
		return "waiting for its dependencies to be done"
	}
	var prevID, prevKind, prevNode string
	var prevStart, prevSeen int64
	var prevKnown bool
	if s.db.QueryRow(`SELECT r.id, r.kind, r.started_at, COALESCE(n.name,''), COALESCE(n.last_seen,0), n.id IS NOT NULL
		FROM runs r LEFT JOIN nodes n ON n.id=r.node_id WHERE r.issue_id=? AND r.status='running' ORDER BY r.started_at LIMIT 1`, i.ID).
		Scan(&prevID, &prevKind, &prevStart, &prevNode, &prevSeen, &prevKnown) == nil {
		where := "on a machine that no longer exists"
		if prevKnown {
			where = "on " + prevNode
			if now()-prevSeen > 70_000 {
				where += fmt.Sprintf(", offline for %s", (time.Duration(now()-prevSeen) * time.Millisecond).Round(time.Minute))
			}
		}
		return fmt.Sprintf("waiting for its previous %s session %s (%s, started %s ago) to stop — it is cancelled and released automatically",
			prevKind, shortID(prevID), where, (time.Duration(now()-prevStart) * time.Millisecond).Round(time.Minute))
	}
	var withRole, capable, online, unpaused, notDraining, free []*Node
	for _, n := range machines {
		if n.HasRole(role) {
			withRole = append(withRole, n)
		}
	}
	if len(withRole) == 0 {
		return fmt.Sprintf("no machine with the %s role is assigned to this project", what)
	}
	for _, n := range withRole {
		if canEverRun(n, i.Requires) {
			capable = append(capable, n)
		}
	}
	if len(capable) == 0 {
		return fmt.Sprintf("no %s machine has all of: %s", what, strings.Join(i.Requires, ", "))
	}
	names := func(ns []*Node) string {
		var out []string
		for _, n := range ns {
			out = append(out, n.Name)
		}
		return strings.Join(out, ", ")
	}
	for _, n := range capable {
		if n.Online {
			online = append(online, n)
		}
	}
	if len(online) == 0 {
		return fmt.Sprintf("the %s machines that can take it are offline (%s)", what, names(capable))
	}
	for _, n := range online {
		if !n.Paused {
			unpaused = append(unpaused, n)
		}
	}
	if len(unpaused) == 0 {
		return fmt.Sprintf("the %s machines that can take it are paused (%s)", what, names(online))
	}
	for _, n := range unpaused {
		if !drainHoldsWork(n.ID) {
			notDraining = append(notDraining, n)
		}
	}
	if len(notDraining) == 0 {
		return fmt.Sprintf("%s: updating, no new work for a few minutes", names(unpaused))
	}
	for _, n := range notDraining {
		if running[n.ID] < n.MaxConc {
			free = append(free, n)
		}
	}
	if len(free) == 0 {
		var busy []string
		for _, n := range notDraining {
			busy = append(busy, fmt.Sprintf("%s %d/%d", n.Name, running[n.ID], n.MaxConc))
		}
		return "all slots busy (" + strings.Join(busy, ", ") + ")"
	}
	if need, _ := parseGPUNeed(i.Requires); need.count > 0 {
		gpuFree := false
		for _, n := range free {
			if _, ok := allocGPUs(n, need, leases[n.ID]); ok {
				gpuFree = true
				break
			}
		}
		if !gpuFree {
			return fmt.Sprintf("waiting for a free GPU (%s)", names(free))
		}
	}
	return "queued: the next free slot takes it (reviews, then priority order)" // claimable right now
}
