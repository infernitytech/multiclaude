package runner

import (
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type cpuSampler struct{}

// pct sums the per-process CPU usage (ps reports % of one core).
func (c *cpuSampler) pct() float64 {
	var sum float64
	for _, l := range strings.Split(cmdOut("ps", "-A", "-o", "%cpu="), "\n") {
		v, _ := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(l, ",", ".")), 64)
		sum += v
	}
	return min(100, sum/float64(runtime.NumCPU()))
}

func loadAvg() float64 {
	// "{ 1.23 1.10 1.05 }"
	f := strings.Fields(strings.Trim(cmdOut("sysctl", "-n", "vm.loadavg"), "{} "))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func memUsage() (usedGB, totalGB float64) {
	total, _ := strconv.ParseFloat(cmdOut("sysctl", "-n", "hw.memsize"), 64)
	out := cmdOut("vm_stat")
	page := 4096.0
	if i := strings.Index(out, "page size of "); i >= 0 {
		if f := strings.Fields(out[i+len("page size of "):]); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				page = v
			}
		}
	}
	pages := map[string]float64{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		n, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "."), 64)
		pages[strings.TrimSpace(k)] = n
	}
	// Like Activity Monitor's "memory used": app (active+wired−purgeable) + compressed.
	used := (pages["Pages active"] + pages["Pages wired down"] + pages["Pages occupied by compressor"] - pages["Pages purgeable"]) * page
	return used / 1e9, total / 1e9
}

// diskUsage covers the system volume and external volumes. APFS volumes of one
// container share space, so only "/" and /Volumes/* are counted.
func diskUsage() (usedGB, totalGB float64) {
	mounts := []string{"/"}
	for _, l := range strings.Split(cmdOut("df", "-l", "-k"), "\n") {
		if i := strings.Index(l, " /Volumes/"); i >= 0 {
			mounts = append(mounts, strings.TrimSpace(l[i:]))
		}
	}
	seen := map[[2]int64]bool{}
	for _, m := range mounts {
		var st syscall.Statfs_t
		if syscall.Statfs(m, &st) != nil || st.Blocks == 0 {
			continue
		}
		key := [2]int64{int64(st.Fsid.Val[0]), int64(st.Fsid.Val[1])}
		if seen[key] {
			continue
		}
		seen[key] = true
		bs := float64(st.Bsize)
		total, free := float64(st.Blocks)*bs, float64(st.Bavail)*bs
		totalGB += total / 1e9
		usedGB += (total - free) / 1e9
	}
	return
}
