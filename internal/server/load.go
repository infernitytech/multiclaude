package server

import (
	"net/http"
	"sync"
	"time"

	"multiclaude/internal/proto"
)

// Machine load: runners report utilisation every 10 s. It is kept in memory
// only and pushed to the web UI as "load" events.

var (
	loadMu sync.Mutex
	loads  = map[string]*proto.Load{}
)

// loadOf returns the last report of a machine, or nil if it is stale.
func loadOf(nodeID string) *proto.Load {
	loadMu.Lock()
	defer loadMu.Unlock()
	l := loads[nodeID]
	if l == nil || time.Since(time.UnixMilli(l.TS)) > time.Minute {
		return nil
	}
	c := *l
	return &c
}

func (s *Server) runnerLoad(w http.ResponseWriter, r *http.Request) {
	var l proto.Load
	if err := readJSON(r, &l); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	id := nodeFrom(r)
	if id == "" {
		httpErr(w, 401, "machine token required")
		return
	}
	l.TS = now() // server clock: machines' clocks may differ
	loadMu.Lock()
	loads[id] = &l
	loadMu.Unlock()
	if l.Account != "" {
		if res, _ := s.db.Exec(`UPDATE nodes SET claude_account=? WHERE id=? AND claude_account<>?`, l.Account, id, l.Account); res != nil {
			if n, _ := res.RowsAffected(); n > 0 {
				s.emit(event{Type: "nodes"})
			}
		}
	}
	s.emit(event{Type: "load", Data: map[string]any{"node_id": id, "load": l}})
	w.WriteHeader(204)
}

// Draining runners: they wait for their sessions to end before updating.
var draining = map[string]string{} // node id → version (guarded by loadMu)

func setDraining(nodeID, version string) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if version == "" {
		delete(draining, nodeID)
	} else {
		draining[nodeID] = version
	}
}

func drainingTo(nodeID string) string {
	loadMu.Lock()
	defer loadMu.Unlock()
	return draining[nodeID]
}
