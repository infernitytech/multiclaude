package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"multiclaude/internal/proto"
)

// GPU scheduling.
//
// A machine can have several GPUs. Issues ask for GPUs in their "requires":
//   gpu / cuda / nvidia / rocm / mps → one GPU
//   gpus:N                          → N GPUs (at least)
//   vram:N                          → every assigned GPU has at least N GB memory
//   blackwell / hopper / ada / cdna3 → every assigned GPU is of that architecture
//   sm_90 / sm>=80 / gfx942         → exact or minimum NVIDIA compute capability, AMD target
// The server hands out GPUs exclusively per run (stored in runs.gpus), so two
// agents on the same machine never train on the same GPU. The runner exposes
// only the assigned GPUs to the session (CUDA_VISIBLE_DEVICES etc.). Sessions
// that don't need a GPU only see the GPUs that were free when they started.
// A human can reserve a single GPU by switching off "gpu#<n>" on the machine.

var gpuFamily = map[string]bool{"gpu": true, "cuda": true, "nvidia": true, "rocm": true, "mps": true}

type gpuNeed struct {
	count  int // 0 = no GPU needed
	vramGB int
	arch   string // architecture tag, e.g. "blackwell"
	sm     int    // exact compute capability ×10 (sm_90 → 90)
	smMin  int    // minimum compute capability ×10 (sm>=80)
	gfx    string // AMD target
}

// gpuArchTags are the architecture names usable in "requires".
var gpuArchTags = map[string]bool{"kepler": true, "maxwell": true, "pascal": true, "volta": true, "turing": true, "ampere": true,
	"ada": true, "hopper": true, "blackwell": true, "cdna": true, "cdna2": true, "cdna3": true, "cdna4": true,
	"rdna": true, "rdna2": true, "rdna3": true, "rdna4": true}

func archTag(g proto.GPU) string {
	if f := strings.Fields(g.Arch); len(f) > 0 && g.Vendor != "apple" {
		return strings.ToLower(f[0])
	}
	return ""
}

// smOf turns a compute capability "8.9" into 89 (0 if unknown).
func smOf(g proto.GPU) int {
	if g.Vendor != "nvidia" {
		return 0
	}
	maj, min, ok := strings.Cut(g.ComputeCap, ".")
	a, e1 := strconv.Atoi(maj)
	b, e2 := strconv.Atoi(min)
	if !ok || e1 != nil || e2 != nil {
		return 0
	}
	return a*10 + b
}

// gpuMatches: the GPU satisfies the architecture part of a need.
func (n gpuNeed) gpuMatches(g proto.GPU) bool {
	if n.arch != "" && archTag(g) != n.arch {
		return false
	}
	if n.sm > 0 && smOf(g) != n.sm {
		return false
	}
	if n.smMin > 0 && smOf(g) < n.smMin {
		return false
	}
	if n.gfx != "" && !strings.EqualFold(g.ComputeCap, n.gfx) {
		return false
	}
	return true
}

// parseGPUNeed extracts the GPU part of a requires list; rest are plain capabilities.
func parseGPUNeed(requires []string) (need gpuNeed, rest []string) {
	for _, r := range requires {
		l := strings.ToLower(strings.TrimSpace(r))
		if k, v, ok := strings.Cut(l, ":"); ok && (k == "gpus" || k == "vram") {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(v, "gb"), "g"))
			if err != nil || n < 1 {
				rest = append(rest, r) // malformed: keep it so it shows up as unmatched
				continue
			}
			if k == "gpus" {
				need.count = max(need.count, n)
			} else {
				need.vramGB = max(need.vramGB, n)
			}
			continue
		}
		switch {
		case gpuArchTags[l]:
			need.arch = l
			continue
		case strings.HasPrefix(l, "sm>=") || strings.HasPrefix(l, "sm_") && strings.HasSuffix(l, "+"):
			if v, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(l, "sm>="), "sm_"), "+")); err == nil && v > 0 {
				need.smMin = v
				continue
			}
		case strings.HasPrefix(l, "sm_"):
			if v, err := strconv.Atoi(l[3:]); err == nil && v > 0 {
				need.sm = v
				continue
			}
		case strings.HasPrefix(l, "gfx") && len(l) > 4:
			need.gfx = l
			continue
		}
		if gpuFamily[l] && need.count == 0 {
			need.count = 1
		}
		rest = append(rest, r)
	}
	if (need.vramGB > 0 || need.arch != "" || need.sm > 0 || need.smMin > 0 || need.gfx != "") && need.count == 0 {
		need.count = 1
	}
	return
}

// nodeGPUs returns the machine's GPUs with vendor-local indices filled in
// (older runners don't report them; nvidia-smi order is assumed).
func nodeGPUs(n *Node) []proto.GPU {
	var hw proto.Hardware
	json.Unmarshal(n.Hardware, &hw)
	seen := map[string]int{}
	for i := range hw.GPUs {
		if !hw.GPUs[i].HasIndex {
			hw.GPUs[i].Index = seen[hw.GPUs[i].Vendor]
		}
		seen[hw.GPUs[i].Vendor]++
	}
	return hw.GPUs
}

// gpuReserved reports whether a human switched off GPU number i ("gpu#i").
func gpuReserved(n *Node, i int) bool {
	tag := fmt.Sprintf("gpu#%d", i)
	for _, d := range n.DisabledCaps {
		if strings.EqualFold(d, tag) {
			return true
		}
	}
	return false
}

// usableGPUs are the GPUs eligible for a need, ignoring current leases.
func usableGPUs(n *Node, need gpuNeed) []int {
	var out []int
	for i, g := range nodeGPUs(n) {
		if gpuReserved(n, i) || (need.vramGB > 0 && g.MemoryMB < need.vramGB*1024-512) || !need.gpuMatches(g) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// canEverRun: the machine has the capabilities and enough suitable GPUs.
func canEverRun(n *Node, requires []string) bool {
	need, rest := parseGPUNeed(requires)
	caps := n.AllCaps()
	if !subset(rest, caps) {
		return false
	}
	if need.count == 0 {
		return true
	}
	if !subset([]string{"gpu"}, caps) {
		return false // GPU work switched off on this machine
	}
	return len(usableGPUs(n, need)) >= need.count
}

// gpuLeases maps GPU number → run id for the machine's running runs.
func (s *Server) gpuLeases(nodeID string) map[int]string {
	out := map[int]string{}
	rows, err := s.db.Query(`SELECT id, gpus FROM runs WHERE node_id=? AND status='running'`, nodeID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, js string
		rows.Scan(&id, &js)
		var g []int
		json.Unmarshal([]byte(js), &g)
		for _, i := range g {
			out[i] = id
		}
	}
	return out
}

// allocGPUs picks GPUs for a need from the free ones; ok=false if not enough are free now.
// The smallest suitable GPUs are taken first so the big ones stay available for big jobs.
func allocGPUs(n *Node, need gpuNeed, leased map[int]string) ([]int, bool) {
	if need.count == 0 {
		return nil, true
	}
	gpus := nodeGPUs(n)
	var free []int
	for _, i := range usableGPUs(n, need) {
		if leased[i] == "" {
			free = append(free, i)
		}
	}
	if len(free) < need.count {
		return nil, false
	}
	sort.SliceStable(free, func(a, b int) bool { return gpus[free[a]].MemoryMB < gpus[free[b]].MemoryMB })
	out := append([]int(nil), free[:need.count]...)
	sort.Ints(out)
	return out, true
}

// gpuSetup tells the runner which GPUs the session may see and tells the agent.
// exclusive=false (no GPU requested): the session sees the GPUs free right now, shared.
func (s *Server) gpuSetup(a *proto.Assignment, n *Node, alloc []int, exclusive bool) {
	gpus := nodeGPUs(n)
	if len(gpus) == 0 {
		return
	}
	leased := s.gpuLeases(n.ID)
	visible := alloc
	if !exclusive {
		visible = nil
		for i := range gpus {
			if leased[i] == "" && !gpuReserved(n, i) {
				visible = append(visible, i)
			}
		}
	}
	a.Env = map[string]string{}
	byVendor := map[string][]string{}
	for _, i := range visible {
		byVendor[gpus[i].Vendor] = append(byVendor[gpus[i].Vendor], strconv.Itoa(gpus[i].Index))
	}
	hasVendor := map[string]bool{}
	for _, g := range gpus {
		hasVendor[g.Vendor] = true
	}
	if hasVendor["nvidia"] {
		a.Env["CUDA_DEVICE_ORDER"] = "PCI_BUS_ID" // CUDA numbering = nvidia-smi numbering
		a.Env["CUDA_VISIBLE_DEVICES"] = strings.Join(byVendor["nvidia"], ",")
	}
	if hasVendor["amd"] {
		a.Env["HIP_VISIBLE_DEVICES"] = strings.Join(byVendor["amd"], ",")
		a.Env["ROCR_VISIBLE_DEVICES"] = strings.Join(byVendor["amd"], ",")
	}

	var b strings.Builder
	b.WriteString("\nGPUs for this session:\n")
	describe := func(list []int) string {
		var parts []string
		for _, i := range list {
			g := gpus[i]
			p := fmt.Sprintf("GPU %d = %s", i, g.Name)
			if d := gpuDetails(g, false); d != "" {
				p += " (" + d + ")"
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, "; ")
	}
	envNote := ""
	if v, ok := a.Env["CUDA_VISIBLE_DEVICES"]; ok {
		if nv := len(byVendor["nvidia"]); nv > 1 {
			envNote = fmt.Sprintf(" CUDA_VISIBLE_DEVICES=%q is set, so inside your processes they are numbered from 0 (cuda:0 … cuda:%d).", v, nv-1)
		} else if nv == 1 {
			envNote = fmt.Sprintf(" CUDA_VISIBLE_DEVICES=%q is set, so inside your processes it is cuda:0.", v)
		}
	} else if v, ok := a.Env["HIP_VISIBLE_DEVICES"]; ok {
		envNote = fmt.Sprintf(" HIP_VISIBLE_DEVICES=%q is set, so inside your processes they are numbered from 0.", v)
	}
	switch {
	case exclusive && len(alloc) == 1:
		fmt.Fprintf(&b, "- Reserved exclusively for you: %s.%s Size batch sizes to its memory.\n", describe(alloc), envNote)
	case exclusive:
		fmt.Fprintf(&b, "- Reserved exclusively for you: %s.%s Use all of them when it helps (e.g. DDP / torchrun --nproc_per_node=%d, model or data parallelism); size batch sizes to their memory.\n", describe(alloc), envNote, len(alloc))
	case len(visible) > 0:
		fmt.Fprintf(&b, "- Your issue did not request a GPU. You may use the currently idle %s for light, short GPU work (tests, smoke runs).%s They are not reserved for you: a GPU job may claim them at any time. Heavy GPU work belongs in an issue with requires gpu or gpus:N.\n", describe(visible), envNote)
	default:
		b.WriteString("- Your issue did not request a GPU and all GPUs of this machine are in use or reserved, so none are visible to you. Put GPU work into an issue with requires gpu or gpus:N.\n")
	}
	if t := buildTargets(gpus, visible); t != "" {
		b.WriteString("- " + t + "\n")
	}
	if len(gpus) > 1 {
		fmt.Fprintf(&b, "- This machine has %d GPUs in total: %s. Don't touch GPUs that are not yours (no --gpus all, no hard-coded device ids).", len(gpus), describe(seq(len(gpus))))
		if ids := byVendor["nvidia"]; len(ids) > 0 {
			fmt.Fprintf(&b, " For Docker use --gpus '\"device=%s\"'.", strings.Join(ids, ","))
		}
		b.WriteString("\n")
	}
	a.SystemPrompt += b.String()
}

func seq(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// gpuDetails: "96 GB, Blackwell, sm_120" — memory and what code must target.
func gpuDetails(g proto.GPU, each bool) string {
	var p []string
	if g.MemoryMB > 0 {
		m := fmt.Sprintf("%d GB", (g.MemoryMB+512)/1024)
		if each {
			m += " each"
		}
		p = append(p, m)
	}
	if g.Arch != "" && g.Vendor != "apple" {
		p = append(p, g.Arch)
	}
	switch {
	case smOf(g) > 0:
		p = append(p, fmt.Sprintf("sm_%d", smOf(g)))
	case g.ComputeCap != "":
		p = append(p, g.ComputeCap)
	}
	return strings.Join(p, ", ")
}

// gpuSummary groups identical GPUs: "4× GPU NVIDIA RTX 6000 Ada (48 GB each, Ada Lovelace, sm_89)".
func gpuSummary(gpus []proto.GPU) []string {
	type key struct {
		name, arch, cc string
		mem            int
	}
	var order []key
	count := map[key]int{}
	first := map[key]proto.GPU{}
	for _, g := range gpus {
		k := key{g.Name, g.Arch, g.ComputeCap, (g.MemoryMB + 512) / 1024}
		if count[k] == 0 {
			order = append(order, k)
			first[k] = g
		}
		count[k]++
	}
	var out []string
	for _, k := range order {
		d := gpuDetails(first[k], count[k] > 1)
		if d != "" {
			d = " (" + d + ")"
		}
		if count[k] > 1 {
			out = append(out, fmt.Sprintf("%d× GPU %s%s", count[k], k.name, d))
		} else {
			out = append(out, fmt.Sprintf("GPU %s%s", k.name, d))
		}
	}
	return out
}

// buildTargets tells the agent which architectures to compile GPU code for.
func buildTargets(gpus []proto.GPU, visible []int) string {
	var sms []int
	var gfx []string
	seenSM, seenGfx := map[int]bool{}, map[string]bool{}
	for _, i := range visible {
		g := gpus[i]
		if v := smOf(g); v > 0 && !seenSM[v] {
			seenSM[v] = true
			sms = append(sms, v)
		}
		if g.Vendor == "amd" && strings.HasPrefix(g.ComputeCap, "gfx") && !seenGfx[g.ComputeCap] {
			seenGfx[g.ComputeCap] = true
			gfx = append(gfx, g.ComputeCap)
		}
	}
	var parts []string
	if len(sms) > 0 {
		var cc, arch, sm []string
		for _, v := range sms {
			cc = append(cc, fmt.Sprintf("%d.%d", v/10, v%10))
			arch = append(arch, fmt.Sprintf("-gencode arch=compute_%d,code=sm_%d", v, v))
			sm = append(sm, fmt.Sprintf("sm_%d", v))
		}
		parts = append(parts, fmt.Sprintf("Compile CUDA code for these GPUs: TORCH_CUDA_ARCH_LIST=%q, CMAKE_CUDA_ARCHITECTURES=%s, nvcc %s (prebuilt wheels/kernels must support %s).",
			strings.Join(cc, ";"), strings.ReplaceAll(strings.Join(cc, ";"), ".", ""), strings.Join(arch, " "), strings.Join(sm, ", ")))
	}
	if len(gfx) > 0 {
		parts = append(parts, fmt.Sprintf("Compile ROCm/HIP code for %s (PYTORCH_ROCM_ARCH=%q, --offload-arch=%s).", strings.Join(gfx, ", "), strings.Join(gfx, ";"), gfx[0]))
	}
	return strings.Join(parts, " ")
}
