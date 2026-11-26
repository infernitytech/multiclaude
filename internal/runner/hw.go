package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"multiclaude/internal/proto"
)

var probeTools = []string{
	"docker", "podman", "python3", "uv", "node", "npm", "bun", "go", "cargo", "rustc", "java", "dotnet",
	"gcc", "clang", "cmake", "make", "nvcc", "git-lfs", "kubectl", "terraform", "ffmpeg", "blender", "unity", "godot",
}

func cmdOut(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectHardware inspects the local machine.
func DetectHardware(workdir string) proto.Hardware {
	hw := proto.Hardware{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCores: runtime.NumCPU()}
	hw.Hostname, _ = os.Hostname()

	switch runtime.GOOS {
	case "linux":
		if f, err := os.Open("/proc/cpuinfo"); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			gotFlags := false
			for sc.Scan() {
				k, v, ok := strings.Cut(sc.Text(), ":")
				if !ok {
					continue
				}
				switch k = strings.TrimSpace(k); {
				case k == "model name" && hw.CPUModel == "":
					hw.CPUModel = strings.TrimSpace(v)
				case (k == "flags" || k == "Features") && !gotFlags: // x86 / arm64
					hw.CPUFeatures, gotFlags = cpuFeatures(strings.Fields(v)), true
				}
				if hw.CPUModel != "" && gotFlags {
					break
				}
			}
			f.Close()
		}
		if f, err := os.Open("/proc/meminfo"); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				fs := strings.Fields(sc.Text())
				if len(fs) >= 2 && fs[0] == "MemTotal:" {
					kb, _ := strconv.ParseFloat(fs[1], 64)
					hw.MemoryGB = round1(kb / 1024 / 1024)
				}
			}
			f.Close()
		}
	case "darwin":
		hw.CPUModel = cmdOut("sysctl", "-n", "machdep.cpu.brand_string")
		if b, err := strconv.ParseFloat(cmdOut("sysctl", "-n", "hw.memsize"), 64); err == nil {
			hw.MemoryGB = round1(b / 1024 / 1024 / 1024)
		}
		if runtime.GOARCH == "arm64" {
			g := proto.GPU{Vendor: "apple", Name: hw.CPUModel + " GPU", Arch: hw.CPUModel}
			// "Total Number of Cores: 40", "Metal Support: Metal 3"
			for _, l := range strings.Split(cmdOut("system_profiler", "SPDisplaysDataType"), "\n") {
				k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
				if !ok {
					continue
				}
				switch strings.TrimSpace(k) {
				case "Total Number of Cores":
					g.Cores, _ = strconv.Atoi(strings.TrimSpace(v))
				case "Metal Support", "Metal Family":
					g.ComputeCap = strings.TrimSpace(v)
				}
			}
			if g.Cores > 0 {
				g.Name = fmt.Sprintf("%s GPU (%d cores)", hw.CPUModel, g.Cores)
			}
			hw.GPUs = append(hw.GPUs, g)
		}
	}

	// NVIDIA (compute_cap needs driver ≥ 510; older drivers get the basic query)
	out := cmdOut("nvidia-smi", "--query-gpu=index,name,memory.total,uuid,compute_cap,driver_version", "--format=csv,noheader,nounits")
	if out == "" {
		out = cmdOut("nvidia-smi", "--query-gpu=index,name,memory.total,uuid", "--format=csv,noheader,nounits")
	}
	if out != "" {
		q := nvidiaQuery()
		for _, l := range strings.Split(out, "\n") {
			parts := strings.Split(l, ",")
			if len(parts) < 4 {
				continue
			}
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			idx, err := strconv.Atoi(parts[0])
			mem, _ := strconv.Atoi(parts[2])
			g := proto.GPU{Vendor: "nvidia", Name: parts[1], MemoryMB: mem, Index: idx, HasIndex: err == nil, UUID: parts[3]}
			if len(parts) >= 6 && parts[4] != "[N/A]" {
				g.ComputeCap, g.Driver = parts[4], parts[5]
			}
			if ni := countVendor(hw.GPUs, "nvidia"); ni < len(q.archs) { // -q lists GPUs in the same order
				g.Arch = q.archs[ni]
			}
			if g.Arch == "" {
				g.Arch = nvidiaArch(g.ComputeCap)
			}
			hw.GPUs = append(hw.GPUs, g)
		}
		hw.CUDAVersion = q.cuda
		if v := cmdOut("nvcc", "--version"); v != "" {
			if i := strings.Index(v, "release "); i >= 0 {
				hw.CUDAToolkit = strings.TrimSuffix(strings.Fields(v[i+8:])[0], ",")
			}
		}
	}
	// AMD (ROCm)
	if out := cmdOut("rocm-smi", "--showproductname", "--csv"); out != "" {
		vram := amdVRAM()
		for i, l := range strings.Split(out, "\n") {
			if i == 0 || strings.TrimSpace(l) == "" {
				continue
			}
			f := strings.Split(l, ",")
			name := "AMD GPU"
			if len(f) > 1 {
				name = strings.TrimSpace(f[1])
			}
			idx := i - 1
			if n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(f[0]), "card")); err == nil {
				idx = n
			}
			hw.GPUs = append(hw.GPUs, proto.GPU{Vendor: "amd", Name: name, Index: idx, HasIndex: true, MemoryMB: vram[idx]})
		}
		// rocminfo lists GPU agents in the same order: "Name: gfx942"
		k := 0
		for _, l := range strings.Split(cmdOut("rocminfo"), "\n") {
			key, v, ok := strings.Cut(strings.TrimSpace(l), ":")
			v = strings.TrimSpace(v)
			if !ok || strings.TrimSpace(key) != "Name" || !strings.HasPrefix(v, "gfx") {
				continue
			}
			for k < len(hw.GPUs) && hw.GPUs[k].Vendor != "amd" {
				k++
			}
			if k < len(hw.GPUs) {
				hw.GPUs[k].ComputeCap, hw.GPUs[k].Arch = v, amdArch(v)
				k++
			}
		}
		if b, err := os.ReadFile("/opt/rocm/.info/version"); err == nil {
			hw.ROCmVersion = strings.TrimSpace(string(b))
		}
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(workdir, &st); err == nil {
		hw.DiskFreeGB = round1(float64(st.Bavail) * float64(uint64(st.Bsize)) / 1024 / 1024 / 1024)
	}
	for _, t := range probeTools {
		if _, err := exec.LookPath(t); err == nil {
			hw.Tools = append(hw.Tools, t)
		}
	}
	return hw
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// Capabilities derives scheduling tags from hardware plus user extras.
func Capabilities(hw proto.Hardware, extra []string) []string {
	set := map[string]bool{hw.OS: true, hw.Arch: true}
	if hw.OS == "darwin" {
		set["macos"] = true
	}
	for _, g := range hw.GPUs {
		set["gpu"] = true
		switch g.Vendor {
		case "nvidia":
			set["cuda"] = true
			set["nvidia"] = true
		case "amd":
			set["rocm"] = true
		case "apple":
			set["mps"] = true
		}
	}
	for _, g := range hw.GPUs {
		for _, t := range gpuTags(g) {
			set[t] = true
		}
	}
	for _, f := range hw.CPUFeatures {
		if f == "avx512" || f == "amx" || f == "sve" || f == "sve2" {
			set[f] = true
		}
	}
	if hw.MemoryGB >= 64 {
		set["bigmem"] = true
	}
	for _, t := range hw.Tools {
		set[t] = true
	}
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			set[e] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FindClaude locates the Claude Code CLI.
func FindClaude(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local/bin/claude"),
		filepath.Join(home, ".claude/local/claude"),
		filepath.Join(home, ".npm-global/bin/claude"),
		"/usr/local/bin/claude", "/opt/homebrew/bin/claude", "/usr/bin/claude",
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// ClaudeAuth guesses how the local Claude Code is authenticated.
func ClaudeAuth() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "ANTHROPIC_API_KEY env"
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return "CLAUDE_CODE_OAUTH_TOKEN env"
	}
	home, _ := os.UserHomeDir()
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = filepath.Join(home, ".claude")
	}
	if _, err := os.Stat(filepath.Join(cfg, ".credentials.json")); err == nil {
		return "subscription (credentials file)"
	}
	if runtime.GOOS == "darwin" {
		return "macOS keychain (assumed)"
	}
	return "not found — run `claude` once and log in"
}

// ClaudeAccount reads the logged-in account from Claude Code's config file.
func ClaudeAccount() string {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return "API key"
	}
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(home, ".claude.json")}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		paths = append([]string{filepath.Join(d, ".claude.json")}, paths...)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			OAuthAccount *struct {
				Email   string `json:"emailAddress"`
				Name    string `json:"displayName"`
				Org     string `json:"organizationName"`
				Plan    string `json:"planDisplayName"`
				Billing string `json:"billingType"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &cfg) != nil || cfg.OAuthAccount == nil || cfg.OAuthAccount.Email == "" {
			continue
		}
		a := cfg.OAuthAccount
		s := a.Email
		if a.Name != "" {
			s = a.Name + " <" + a.Email + ">"
		}
		var extra []string
		if a.Plan != "" {
			extra = append(extra, a.Plan)
		}
		if a.Org != "" && !strings.Contains(a.Org, a.Email) {
			extra = append(extra, a.Org)
		}
		if len(extra) > 0 {
			s += " · " + strings.Join(extra, " · ")
		}
		return s
	}
	return ""
}

// amdVRAM returns total VRAM in MB per rocm-smi device number.
func amdVRAM() map[int]int {
	out := map[int]int{}
	for i, l := range strings.Split(cmdOut("rocm-smi", "--showmeminfo", "vram", "--csv"), "\n") {
		f := strings.Split(l, ",")
		if i == 0 || len(f) < 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(f[0]), "card"))
		b, err2 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		if err == nil && err2 == nil {
			out[n] = int(b / 1024 / 1024)
		}
	}
	return out
}

func countVendor(gpus []proto.GPU, vendor string) int {
	n := 0
	for _, g := range gpus {
		if g.Vendor == vendor {
			n++
		}
	}
	return n
}

type nvq struct {
	archs []string // "Product Architecture" per GPU
	cuda  string
}

// nvidiaQuery reads details only `nvidia-smi -q` has.
func nvidiaQuery() nvq {
	var q nvq
	for _, l := range strings.Split(cmdOut("nvidia-smi", "-q"), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(strings.SplitN(v, "[", 2)[0]) // drop "[Deprecated …]" notes
		switch strings.TrimSpace(k) {
		case "Product Architecture":
			q.archs = append(q.archs, v)
		case "CUDA Version", "CUDA UMD Version":
			if q.cuda == "" {
				q.cuda = v
			}
		}
	}
	return q
}

// nvidiaArch maps a compute capability to the architecture family.
func nvidiaArch(cc string) string {
	major, minor, _ := strings.Cut(cc, ".")
	switch major {
	case "3":
		return "Kepler"
	case "5":
		return "Maxwell"
	case "6":
		return "Pascal"
	case "7":
		if minor == "5" {
			return "Turing"
		}
		return "Volta"
	case "8":
		if minor == "9" {
			return "Ada Lovelace"
		}
		return "Ampere"
	case "9":
		return "Hopper"
	case "10", "11", "12":
		return "Blackwell"
	}
	return ""
}

// amdArch maps a gfx target to the architecture family.
func amdArch(gfx string) string {
	switch {
	case strings.HasPrefix(gfx, "gfx950"):
		return "CDNA4"
	case strings.HasPrefix(gfx, "gfx94"):
		return "CDNA3"
	case strings.HasPrefix(gfx, "gfx90a"):
		return "CDNA2"
	case strings.HasPrefix(gfx, "gfx908"):
		return "CDNA"
	case strings.HasPrefix(gfx, "gfx12"):
		return "RDNA4"
	case strings.HasPrefix(gfx, "gfx11"):
		return "RDNA3"
	case strings.HasPrefix(gfx, "gfx103"):
		return "RDNA2"
	case strings.HasPrefix(gfx, "gfx101"):
		return "RDNA"
	case strings.HasPrefix(gfx, "gfx9"):
		return "GCN5 (Vega)"
	}
	return ""
}

// cpuFeatures picks the instruction-set extensions that matter for builds and
// performance work from /proc/cpuinfo flags (x86) or Features (arm64).
func cpuFeatures(flags []string) []string {
	has := map[string]bool{}
	for _, f := range flags {
		has[f] = true
	}
	var out []string
	add := func(name string, any ...string) {
		for _, f := range any {
			if has[f] {
				out = append(out, name)
				return
			}
		}
	}
	add("avx2", "avx2")
	add("avx512", "avx512f")
	add("avx512-bf16", "avx512_bf16")
	add("avx512-fp16", "avx512_fp16")
	add("avx-vnni", "avx_vnni", "avx512_vnni")
	add("amx", "amx_tile")
	add("neon", "asimd")
	add("sve", "sve")
	add("sve2", "sve2")
	add("sme", "sme")
	add("bf16", "bf16")
	add("i8mm", "i8mm")
	return out
}

// gpuTags are capability tags for routing by GPU generation: the architecture
// ("blackwell", "ada", "hopper", "cdna3") and the target ("sm_120", "gfx942").
func gpuTags(g proto.GPU) []string {
	var out []string
	if g.Vendor != "apple" && g.Arch != "" {
		out = append(out, strings.ToLower(strings.Fields(g.Arch)[0]))
	}
	if g.Vendor == "nvidia" && g.ComputeCap != "" {
		out = append(out, "sm_"+strings.ReplaceAll(g.ComputeCap, ".", ""))
	}
	if g.Vendor == "amd" && strings.HasPrefix(g.ComputeCap, "gfx") {
		out = append(out, g.ComputeCap)
	}
	return out
}
