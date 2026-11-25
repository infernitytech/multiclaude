package server

import (
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"multiclaude/internal/proto"
)

//go:embed web
var webFS embed.FS

type Server struct {
	DataDir     string
	Addr        string
	DownloadDir string // directory holding prebuilt runner binaries

	db  *sql.DB
	hub *hub

	mu      sync.Mutex
	queues  map[string][]proto.Command
	notify  map[string]chan struct{}
	workCh  chan struct{} // closed & replaced whenever new work may be claimable
	replies map[string]int
}

func New(dataDir, addr, downloadDir string) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		return nil, err
	}
	db, err := openDB(filepath.Join(dataDir, "multiclaude.db"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		DataDir: dataDir, Addr: addr, DownloadDir: downloadDir, db: db,
		hub:    newHub(),
		queues: map[string][]proto.Command{},
		notify: map[string]chan struct{}{},
		workCh: make(chan struct{}),
	}
	// Re-apply repo config for existing repos (e.g. after upgrades).
	rows, err := db.Query(`SELECT slug FROM projects`)
	if err == nil {
		for rows.Next() {
			var slug string
			rows.Scan(&slug)
			configureRepo(s.repoPath(slug))
		}
		rows.Close()
	}
	return s, nil
}

func (s *Server) logf(f string, a ...any) { log.Printf(f, a...) }

func (s *Server) listenPort() string {
	_, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return ":8080"
	}
	return ":" + port
}

func fileModTime(f *os.File) time.Time {
	if st, err := f.Stat(); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func (s *Server) Run() error {
	go s.reaper()
	mux := http.NewServeMux()
	s.routes(mux)
	srv := &http.Server{Addr: s.Addr, Handler: logMiddleware(mux), ReadHeaderTimeout: 30 * time.Second}
	log.Printf("MultiClaude server %s listening on %s (data: %s)", proto.Version, s.Addr, s.DataDir)
	return srv.ListenAndServe()
}

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("MC_DEBUG") != "" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) routes(m *http.ServeMux) {
	m.HandleFunc("/git/", s.handleGit)

	// UI API
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("GET /api/overview", s.apiOverview)
	m.HandleFunc("GET /api/projects", s.apiProjects)
	m.HandleFunc("POST /api/projects", s.apiCreateProject)
	m.HandleFunc("GET /api/projects/{slug}", s.apiProject)
	m.HandleFunc("PATCH /api/projects/{slug}", s.apiUpdateProject)
	m.HandleFunc("DELETE /api/projects/{slug}", s.apiDeleteProject)
	m.HandleFunc("GET /api/projects/{slug}/issues", s.apiIssues)
	m.HandleFunc("POST /api/projects/{slug}/issues", s.apiCreateIssue)
	m.HandleFunc("GET /api/projects/{slug}/issues/{n}", s.apiIssue)
	m.HandleFunc("PATCH /api/projects/{slug}/issues/{n}", s.apiUpdateIssue)
	m.HandleFunc("DELETE /api/projects/{slug}/issues/{n}", s.apiDeleteIssue)
	m.HandleFunc("POST /api/projects/{slug}/issues/{n}/comments", s.apiComment)
	m.HandleFunc("POST /api/projects/{slug}/issues/{n}/merge", s.apiMerge)
	m.HandleFunc("POST /api/projects/{slug}/issues/{n}/request-changes", s.apiRequestChanges)
	m.HandleFunc("GET /api/projects/{slug}/messages", s.apiMessages)
	m.HandleFunc("POST /api/projects/{slug}/messages", s.apiPostMessage)
	m.HandleFunc("GET /api/projects/{slug}/runs", s.apiProjectRuns)
	m.HandleFunc("GET /api/projects/{slug}/repo/branches", s.apiBranches)
	m.HandleFunc("GET /api/projects/{slug}/repo/tree", s.apiTree)
	m.HandleFunc("GET /api/projects/{slug}/repo/blob", s.apiBlob)
	m.HandleFunc("GET /api/projects/{slug}/repo/log", s.apiLog)
	m.HandleFunc("GET /api/projects/{slug}/repo/commit", s.apiCommit)
	m.HandleFunc("GET /api/nodes", s.apiNodes)
	m.HandleFunc("PATCH /api/nodes/{id}", s.apiUpdateNode)
	m.HandleFunc("DELETE /api/nodes/{id}", s.apiDeleteNode)
	m.HandleFunc("PUT /api/nodes/{id}/projects/{slug}", s.apiAssignNode)
	m.HandleFunc("DELETE /api/nodes/{id}/projects/{slug}", s.apiUnassignNode)
	m.HandleFunc("GET /api/runs/{id}", s.apiRun)
	m.HandleFunc("GET /api/runs/{id}/events", s.apiRunEvents)
	m.HandleFunc("POST /api/runs/{id}/message", s.apiRunMessage)
	m.HandleFunc("POST /api/runs/{id}/cancel", s.apiRunCancel)

	// Runner API
	m.HandleFunc("POST /api/runner/register", s.runnerRegister)
	m.HandleFunc("POST /api/runner/poll", s.runnerPoll)
	m.HandleFunc("POST /api/runner/runs/{id}/events", s.runnerEvents)
	m.HandleFunc("POST /api/runner/runs/{id}/finish", s.runnerFinish)
	m.HandleFunc("POST /api/agent/call", s.agentCall)

	// Runner distribution
	m.HandleFunc("GET /install.sh", s.installScript)
	m.HandleFunc("GET /download/{file}", s.download)

	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// SPA: anything that isn't a static asset gets index.html
		if r.URL.Path != "/" && !strings.Contains(filepath.Base(r.URL.Path), ".") {
			r.URL.Path = "/"
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// ---- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 50<<20)).Decode(v)
}

// ---- event hub (SSE) --------------------------------------------------------

type event struct {
	Type      string `json:"type"`
	ProjectID int64  `json:"project_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (s *Server) emit(e event) {
	b, _ := json.Marshal(e)
	s.hub.mu.Lock()
	for c := range s.hub.subs {
		select {
		case c <- b:
		default: // slow client: drop
		}
	}
	s.hub.mu.Unlock()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	c := make(chan []byte, 512)
	s.hub.mu.Lock()
	s.hub.subs[c] = struct{}{}
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, c)
		s.hub.mu.Unlock()
	}()
	w.Write([]byte("retry: 2000\n\n"))
	fl.Flush()
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-c:
			w.Write([]byte("data: "))
			w.Write(b)
			w.Write([]byte("\n\n"))
			fl.Flush()
		case <-t.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

// ---- node command queues ------------------------------------------------------

func (s *Server) enqueue(nodeID string, c proto.Command) {
	s.mu.Lock()
	s.queues[nodeID] = append(s.queues[nodeID], c)
	ch := s.notify[nodeID]
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) drain(nodeID string) []proto.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.queues[nodeID]
	delete(s.queues, nodeID)
	return q
}

// workAvailable wakes all pollers so they re-try claiming.
func (s *Server) workAvailable() {
	s.mu.Lock()
	close(s.workCh)
	s.workCh = make(chan struct{})
	s.mu.Unlock()
}

func (s *Server) onPush(slug string) {
	var id int64
	s.db.QueryRow(`SELECT id FROM projects WHERE slug=?`, slug).Scan(&id)
	s.emit(event{Type: "repo", ProjectID: id})
}

// reaper releases work held by nodes that disappeared.
func (s *Server) reaper() {
	t := time.NewTicker(15 * time.Second)
	for range t.C {
		cutoff := now() - 90_000
		rows, err := s.db.Query(`SELECT r.id FROM runs r JOIN nodes n ON n.id=r.node_id WHERE r.status='running' AND n.last_seen < ?`, cutoff)
		if err != nil {
			continue
		}
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			s.finishRun(id, proto.RunFinish{Status: "lost", Error: "machine went offline"})
		}
	}
}
