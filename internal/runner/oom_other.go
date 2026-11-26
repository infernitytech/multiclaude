//go:build !linux

package runner

func protectFromOOM(pid int) {}
func ensureOOMPolicy()       {}
