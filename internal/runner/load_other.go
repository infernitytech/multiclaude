//go:build !linux && !darwin

package runner

type cpuSampler struct{}

func (c *cpuSampler) pct() float64         { return 0 }
func loadAvg() float64                     { return 0 }
func memUsage() (usedGB, totalGB float64)  { return }
func diskUsage() (usedGB, totalGB float64) { return }
