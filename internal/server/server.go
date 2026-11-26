package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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

	"golang.org/x/crypto/acme/autocert"
)

//go:embed web
var webFS embed.FS

type Server struct {
	DataDir     string
	Addr        string
	DownloadDir string // directory holding prebuilt runner binaries

	// HTTPS (optional). Domains → automatic Let's Encrypt certificates;
	// CertFile/KeyFile → your own certificate. TLSAddr is the HTTPS listener.
	Domains   []string
	ACMEEmail string
	CertFile  string
	KeyFile   string
	TLSAddr   string
	// DNS-01 validation (any port works): provider "ionos" + its API key.
	DNSProvider   string
	DNSAPIKey     string
	ACMEDirectory string // default: Let's Encrypt production
	DynDNS        bool   // keep the domains' A records at our public IP

	db  *sql.DB
	hub *hub

	mu      sync.Mutex
	queues  map[string][]proto.Command
	notify  map[string]chan struct{}
	workCh  chan struct{} // closed & replaced whenever new work may be claimable
	replies map[string]int

	internalToken string // lets the server talk to its own git endpoint
}

func New(dataDir, addr, downloadDir string) (*Server, error) {
	// Must be absolute: git http-backend runs as CGI with its working directory
	// set to git's own directory, so a relative GIT_PROJECT_ROOT would not resolve.
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if downloadDir != "" {
		if downloadDir, err = filepath.Abs(downloadDir); err != nil {
			return nil, err
		}
	}
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

		internalToken: randToken("mci_"),
	}
	s.ensurePrimaryRepos()
	// Re-apply repo config for existing repos (e.g. after upgrades).
	rows, err := db.Query(`SELECT CASE WHEN r.is_primary=1 THEN p.slug ELSE p.slug || '/' || r.name END FROM repos r JOIN projects p ON p.id=r.project_id`)
	if err == nil {
		var keys []string
		for rows.Next() {
			var k string
			rows.Scan(&k)
			keys = append(keys, k)
		}
		rows.Close()
		for _, k := range keys {
			configureRepo(s.repoPath(k))
		}
	}
	return s, nil
}

func (s *Server) logf(f string, a ...any) { log.Printf(f, a...) }

func (s *Server) listenPort() string {
	_, port, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return ":8090"
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
	s.ensureAdmin()
	s.joinToken()
	go s.reaper()
	go s.mirrorLoop()
	go s.updateCheckLoop()
	mux := http.NewServeMux()
	s.routes(mux)
	handler := logMiddleware(s.authMiddleware(mux))
	newSrv := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 5 * time.Minute}
	}
	errc := make(chan error, 2)
	httpHandler := handler
	if s.DynDNS {
		if s.DNSProvider != "ionos" || len(s.Domains) == 0 {
			return errors.New("dynamic DNS needs MC_DOMAIN and MC_DNS_PROVIDER=ionos")
		}
		go runDynDNS(context.Background(), newIONOS(s.DNSAPIKey), s.Domains)
	}
	switch {
	case len(s.Domains) > 0 && s.DNSProvider != "":
		var p dnsProvider
		switch s.DNSProvider {
		case "ionos":
			if s.DNSAPIKey == "" {
				return errors.New("MC_DNS_API_KEY is required for MC_DNS_PROVIDER=ionos")
			}
			p = newIONOS(s.DNSAPIKey)
		case "cloudflare":
			if s.DNSAPIKey == "" {
				return errors.New("MC_DNS_API_KEY (a Cloudflare API token) is required for MC_DNS_PROVIDER=cloudflare")
			}
			p = newCloudflare(s.DNSAPIKey)
		default:
			return fmt.Errorf("unknown DNS provider %q (supported: cloudflare, ionos)", s.DNSProvider)
		}
		m := newDNSCertManager(s.Domains, s.ACMEEmail, filepath.Join(s.DataDir, "certs"), s.ACMEDirectory, p)
		m.Start(context.Background())
		tsrv := newSrv(s.TLSAddr, handler)
		tsrv.TLSConfig = &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12}
		go func() { errc <- tsrv.ListenAndServeTLS("", "") }()
		log.Printf("HTTPS on %s for %s (Let's Encrypt via %s DNS)", s.TLSAddr, strings.Join(s.Domains, ", "), s.DNSProvider)
	case len(s.Domains) > 0:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(s.Domains...),
			Cache:      autocert.DirCache(filepath.Join(s.DataDir, "certs")),
			Email:      s.ACMEEmail,
		}
		tsrv := newSrv(s.TLSAddr, handler)
		tsrv.TLSConfig = m.TLSConfig() // includes the TLS-ALPN-01 challenge: only the HTTPS port must be reachable
		go func() { errc <- tsrv.ListenAndServeTLS("", "") }()
		httpHandler = m.HTTPHandler(handler) // also answers HTTP-01 challenges; other requests are served normally
		log.Printf("HTTPS on %s with Let's Encrypt certificates for %s (TLS-ALPN challenge: external port 443 must reach this port)", s.TLSAddr, strings.Join(s.Domains, ", "))
	case s.CertFile != "":
		tsrv := newSrv(s.TLSAddr, handler)
		go func() { errc <- tsrv.ListenAndServeTLS(s.CertFile, s.KeyFile) }()
		log.Printf("HTTPS on %s with certificate %s", s.TLSAddr, s.CertFile)
	}
	if s.Addr != "" && s.Addr != "off" {
		go func() { errc <- newSrv(s.Addr, httpHandler).ListenAndServe() }()
		log.Printf("HTTP on %s", s.Addr)
	}
	log.Printf("MultiClaude server %s (data: %s)", proto.Version, s.DataDir)
	return <-errc
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

	// Auth
	m.HandleFunc("GET /api/auth", s.apiAuth)
	m.HandleFunc("/api/login", s.apiLogin)
	m.HandleFunc("POST /api/logout", s.apiLogout)
	m.HandleFunc("GET /api/users", s.apiUsers)
	m.HandleFunc("POST /api/users", s.apiCreateUser)
	m.HandleFunc("PATCH /api/users/{id}", s.apiUpdateUser)
	m.HandleFunc("DELETE /api/users/{id}", s.apiDeleteUser)
	m.HandleFunc("GET /api/runner-setup", s.apiRunnerSetup)
	m.HandleFunc("POST /api/runner-setup/rotate", s.apiRunnerSetup)

	// UI API
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("GET /api/overview", s.apiOverview)
	m.HandleFunc("GET /api/projects", s.apiProjects)
	m.HandleFunc("POST /api/projects", s.apiCreateProject)
	m.HandleFunc("GET /api/projects/{slug}", s.apiProject)
	m.HandleFunc("PATCH /api/projects/{slug}", s.apiUpdateProject)
	m.HandleFunc("DELETE /api/projects/{slug}", s.apiDeleteProject)
	m.HandleFunc("GET /api/projects/{slug}/issues", s.apiIssues)
	m.HandleFunc("GET /api/projects/{slug}/board", s.apiBoard)
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
	m.HandleFunc("GET /api/projects/{slug}/repos", s.apiRepos)
	m.HandleFunc("POST /api/projects/{slug}/repos", s.apiCreateRepo)
	m.HandleFunc("PATCH /api/projects/{slug}/repos/{repo}", s.apiUpdateRepo)
	m.HandleFunc("DELETE /api/projects/{slug}/repos/{repo}", s.apiDeleteRepo)
	m.HandleFunc("POST /api/projects/{slug}/repos/{repo}/mirrors", s.apiCreateMirror)
	m.HandleFunc("PATCH /api/projects/{slug}/repos/{repo}/mirrors/{mid}", s.apiUpdateMirror)
	m.HandleFunc("DELETE /api/projects/{slug}/repos/{repo}/mirrors/{mid}", s.apiDeleteMirror)
	m.HandleFunc("POST /api/projects/{slug}/repos/{repo}/mirrors/{mid}/sync", s.apiSyncMirror)
	m.HandleFunc("GET /api/settings/github", s.apiGitHubSettings)
	m.HandleFunc("PUT /api/settings/github", s.apiGitHubSettings)
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

// onPush runs after anything changed refs in a repo (git push or a server merge).
func (s *Server) onPush(key string) {
	var id int64
	s.db.QueryRow(`SELECT id FROM projects WHERE slug=?`, strings.SplitN(key, "/", 2)[0]).Scan(&id)
	s.emit(event{Type: "repo", ProjectID: id})
	s.mirrorsChanged(key)
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
