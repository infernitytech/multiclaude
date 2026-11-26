package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"multiclaude/internal/proto"
)

func testNode(disabled ...string) *Node {
	hw, _ := json.Marshal(proto.Hardware{GPUs: []proto.GPU{
		{Vendor: "nvidia", Name: "RTX 4090", MemoryMB: 24564},
		{Vendor: "nvidia", Name: "RTX 4090", MemoryMB: 24564},
		{Vendor: "nvidia", Name: "H100", MemoryMB: 81559},
		{Vendor: "nvidia", Name: "H100", MemoryMB: 81559},
	}})
	return &Node{Hardware: hw, Caps: []string{"linux", "gpu", "cuda"}, DisabledCaps: disabled}
}

func TestParseGPUNeed(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want gpuNeed
		rest []string
	}{
		{[]string{"docker"}, gpuNeed{}, []string{"docker"}},
		{[]string{"cuda"}, gpuNeed{count: 1}, []string{"cuda"}},
		{[]string{"gpus:4", "cuda"}, gpuNeed{count: 4}, []string{"cuda"}},
		{[]string{"vram:48GB"}, gpuNeed{count: 1, vramGB: 48}, nil},
		{[]string{"gpus:x"}, gpuNeed{}, []string{"gpus:x"}},
	} {
		need, rest := parseGPUNeed(c.in)
		if need != c.want || !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("parseGPUNeed(%v) = %+v %v", c.in, need, rest)
		}
	}
}

func TestAllocGPUs(t *testing.T) {
	n := testNode()
	if g, _ := allocGPUs(n, gpuNeed{count: 1}, nil); !reflect.DeepEqual(g, []int{0}) {
		t.Errorf("1 GPU: %v (want the smallest)", g)
	}
	if g, _ := allocGPUs(n, gpuNeed{count: 1, vramGB: 80}, nil); !reflect.DeepEqual(g, []int{2}) {
		t.Errorf("vram:80: %v", g)
	}
	if _, ok := allocGPUs(n, gpuNeed{count: 2, vramGB: 80}, map[int]string{3: "r"}); ok {
		t.Error("2×80GB with one H100 busy must wait")
	}
	if g, _ := allocGPUs(testNode("gpu#0"), gpuNeed{count: 3}, nil); !reflect.DeepEqual(g, []int{1, 2, 3}) {
		t.Errorf("reserved gpu#0: %v", g)
	}
	if canEverRun(n, []string{"gpus:5"}) || !canEverRun(n, []string{"gpus:4"}) || canEverRun(testNode("gpu"), []string{"gpu"}) {
		t.Error("canEverRun")
	}
	if s := strings.Join(gpuSummary(nodeGPUs(n)), ", "); s != "2× GPU RTX 4090 (24 GB each), 2× GPU H100 (80 GB each)" {
		t.Error(s)
	}
}

func TestGPUArchNeeds(t *testing.T) {
	hw, _ := json.Marshal(proto.Hardware{GPUs: []proto.GPU{
		{Vendor: "nvidia", Name: "RTX 4090", MemoryMB: 24564, Arch: "Ada Lovelace", ComputeCap: "8.9"},
		{Vendor: "nvidia", Name: "H100", MemoryMB: 81559, Arch: "Hopper", ComputeCap: "9.0"},
		{Vendor: "nvidia", Name: "RTX PRO 6000", MemoryMB: 97887, Arch: "Blackwell", ComputeCap: "12.0"},
	}})
	n := &Node{Hardware: hw, Caps: []string{"linux", "gpu", "cuda"}}
	for _, c := range []struct {
		req  []string
		want []int
	}{
		{[]string{"hopper"}, []int{1}},
		{[]string{"Blackwell"}, []int{2}},
		{[]string{"sm_89"}, []int{0}},
		{[]string{"sm>=90"}, []int{1}}, // smallest suitable first
		{[]string{"sm_90+", "gpus:2"}, []int{1, 2}},
		{[]string{"ada", "vram:48"}, nil}, // no 48 GB Ada GPU
	} {
		need, rest := parseGPUNeed(c.req)
		if len(rest) != 0 {
			t.Errorf("%v: rest %v", c.req, rest)
		}
		got, _ := allocGPUs(n, need, nil)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v want %v", c.req, got, c.want)
		}
		if canEverRun(n, c.req) != (c.want != nil) {
			t.Errorf("%v: canEverRun", c.req)
		}
	}
	if s := strings.Join(gpuSummary(nodeGPUs(n)), ", "); !strings.Contains(s, "GPU H100 (80 GB, Hopper, sm_90)") {
		t.Error(s)
	}
}
