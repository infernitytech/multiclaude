package runner

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"multiclaude/internal/proto"
)

// loadLoop reports the machine's utilisation every 10 seconds.
func (r *Runner) loadLoop(ctx context.Context) {
	var cpu cpuSampler
	cpu.pct() // prime the counters
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		l := sampleLoad(&cpu)
		err := r.postCtx(ctx, "/api/runner/load", l, nil)
		var he *httpError
		if errors.As(err, &he) && he.code == 404 { // older server: try again much later
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
	}
}

func sampleLoad(cpu *cpuSampler) proto.Load {
	l := proto.Load{TS: time.Now().UnixMilli(), CPUPct: round1(cpu.pct()), Load1: round1(loadAvg())}
	used, total := memUsage()
	l.MemUsedGB, l.MemTotalGB = round1(used), round1(total)
	du, dt := diskUsage()
	l.DiskUsedGB, l.DiskTotalGB = math.Round(du), math.Round(dt)
	l.GPUs = gpuLoad()
	l.Account = ClaudeAccount()
	return l
}

// gpuLoad queries utilisation and memory of NVIDIA and AMD GPUs.
func gpuLoad() []proto.GPULoad {
	var out []proto.GPULoad
	for _, l := range strings.Split(cmdOut("nvidia-smi", "--query-gpu=index,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 4 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		util, _ := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		used, _ := strconv.Atoi(strings.TrimSpace(f[2]))
		total, _ := strconv.Atoi(strings.TrimSpace(f[3]))
		out = append(out, proto.GPULoad{Vendor: "nvidia", Index: idx, UtilPct: util, MemUsedMB: used, MemTotalMB: total})
	}
	// rocm-smi CSV: device,GPU use (%),... and VRAM Total/Used Memory (B)
	lines := strings.Split(cmdOut("rocm-smi", "--showuse", "--showmeminfo", "vram", "--csv"), "\n")
	if len(lines) > 1 {
		hdr := strings.Split(lines[0], ",")
		col := func(name string) int {
			for i, h := range hdr {
				if strings.Contains(strings.ToLower(h), name) {
					return i
				}
			}
			return -1
		}
		cu, ct, cm := col("gpu use"), col("total memory"), col("total used memory")
		for _, l := range lines[1:] {
			f := strings.Split(l, ",")
			idx, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(f[0]), "card"))
			if err != nil {
				continue
			}
			g := proto.GPULoad{Vendor: "amd", Index: idx}
			num := func(c int) float64 {
				if c < 0 || c >= len(f) {
					return 0
				}
				v, _ := strconv.ParseFloat(strings.TrimSpace(f[c]), 64)
				return v
			}
			g.UtilPct = num(cu)
			g.MemTotalMB, g.MemUsedMB = int(num(ct)/1024/1024), int(num(cm)/1024/1024)
			out = append(out, g)
		}
	}
	return out
}
