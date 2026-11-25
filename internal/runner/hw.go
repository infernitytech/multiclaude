package runner

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"context"

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
			for sc.Scan() {
				if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
					hw.CPUModel = strings.TrimSpace(v)
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
			hw.GPUs = append(hw.GPUs, proto.GPU{Vendor: "apple", Name: hw.CPUModel + " GPU"})
		}
	}

	// NVIDIA
	if out := cmdOut("nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"); out != "" {
		for _, l := range strings.Split(out, "\n") {
			parts := strings.Split(l, ",")
			if len(parts) >= 2 {
				mem, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
				hw.GPUs = append(hw.GPUs, proto.GPU{Vendor: "nvidia", Name: strings.TrimSpace(parts[0]), MemoryMB: mem})
			}
		}
	}
	// AMD (ROCm)
	if out := cmdOut("rocm-smi", "--showproductname", "--csv"); out != "" {
		for i, l := range strings.Split(out, "\n") {
			if i == 0 || strings.TrimSpace(l) == "" {
				continue
			}
			f := strings.Split(l, ",")
			name := "AMD GPU"
			if len(f) > 1 {
				name = strings.TrimSpace(f[1])
			}
			hw.GPUs = append(hw.GPUs, proto.GPU{Vendor: "amd", Name: name})
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
