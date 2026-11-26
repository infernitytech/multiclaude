package runner

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

type cpuSampler struct{ idle, total uint64 }

// pct is the CPU utilisation since the previous call, from /proc/stat.
func (c *cpuSampler) pct() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	line := strings.SplitN(string(b), "\n", 2)[0]
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0
	}
	var idle, total uint64
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		if i == 7 || i == 8 { // guest time is already counted in user/nice
			continue
		}
		total += n
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	di, dt := idle-c.idle, total-c.total
	first := c.total == 0
	c.idle, c.total = idle, total
	if first || dt == 0 {
		return 0
	}
	return 100 * float64(dt-di) / float64(dt)
}

func loadAvg() float64 {
	b, _ := os.ReadFile("/proc/loadavg")
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

func memUsage() (usedGB, totalGB float64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	var total, avail float64
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64) // kB
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return (total - avail) / 1024 / 1024, total / 1024 / 1024
}

// realFS are filesystems that live on local disks.
var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true, "jfs": true, "reiserfs": true, "bcachefs": true, "hfsplus": true}

// diskUsage sums all local disks. Mounts of the same device (bind mounts,
// btrfs subvolumes) and datasets of the same ZFS pool are counted once.
func diskUsage() (usedGB, totalGB float64) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		dev, mnt := f[0], strings.ReplaceAll(f[1], `\040`, " ")
		if f[2] == "zfs" {
			dev = "zfs:" + strings.SplitN(dev, "/", 2)[0]
		}
		if seen[dev] {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mnt, &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[dev] = true
		bs := float64(st.Bsize)
		total, free := float64(st.Blocks)*bs, float64(st.Bavail)*bs
		if f[2] == "zfs" {
			// A dataset only reports its own usage; the pool's root dataset covers all of them.
			if u, a, ok := zfsPool(dev[4:]); ok {
				total, free = u+a, a
			}
		}
		totalGB += total / 1e9
		usedGB += (total - free) / 1e9
	}
	return
}

// zfsPool returns used and available bytes of a pool (usable space, after parity).
func zfsPool(pool string) (used, avail float64, ok bool) {
	f := strings.Fields(cmdOut("zfs", "list", "-Hp", "-o", "used,avail", pool))
	if len(f) != 2 {
		return 0, 0, false
	}
	u, e1 := strconv.ParseFloat(f[0], 64)
	a, e2 := strconv.ParseFloat(f[1], 64)
	return u, a, e1 == nil && e2 == nil
}
