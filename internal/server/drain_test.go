package server

import (
	"testing"
	"time"
)

func TestDrainHoldsWorkIsBounded(t *testing.T) {
	const id = "node-drain-test"
	defer setDraining(id, "")
	if drainHoldsWork(id) {
		t.Fatal("a machine that isn't draining must get work")
	}
	setDraining(id, "v2")
	if !drainHoldsWork(id) {
		t.Fatal("a fresh drain holds back new work")
	}
	setDraining(id, "v2") // repeated polls keep the original start
	loadMu.Lock()
	drainStart[id] = time.Now().Add(-drainHold - time.Minute)
	loadMu.Unlock()
	if drainHoldsWork(id) {
		t.Fatal("after drainHold the machine gets work again")
	}
	loadMu.Lock()
	drainStart[id] = time.Now().Add(-drainHold - drainPause - time.Minute)
	loadMu.Unlock()
	if !drainHoldsWork(id) {
		t.Fatal("after the pause a new drain window starts")
	}
	setDraining(id, "")
	if drainHoldsWork(id) {
		t.Fatal("drain cleared")
	}
}
