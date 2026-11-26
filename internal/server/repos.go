package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A project has one or more git repositories. The primary repo keeps the
// historic location (/git/<project>.git, data/repos/<project>.git); further
// repos live at /git/<project>/<name>.git. The "key" (slug or slug/name)
// identifies a repo on disk and in URLs.
//
// Each repo can be pushed to any number of mirrors (e.g. a public and a
// private GitHub repo). MultiClaude stays the source of truth: mirrors are
// updated after every push/merge.

const repoSchema = `
CREATE TABLE IF NOT EXISTS repos(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  default_branch TEXT NOT NULL DEFAULT 'main',
  is_primary INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(project_id, name)
);
CREATE TABLE IF NOT EXISTS mirrors(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repo_id INTEGER NOT NULL,
  url TEXT NOT NULL,
  token TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT 'default',
  visibility TEXT NOT NULL DEFAULT 'unknown',
  enabled INTEGER NOT NULL DEFAULT 1,
  last_status TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  last_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
`

type Repo struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"project_id"`
	Name          string    `json:"name"`
	Key           string    `json:"key"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	Primary       bool      `json:"primary"`
	CloneURL      string    `json:"clone_path"` // relative to the server URL
	Mirrors       []*Mirror `json:"mirrors"`
}

type Mirror struct {
	ID         int64  `json:"id"`
	RepoID     int64  `json:"repo_id"`
	URL        string `json:"url"`
	Mode       string `json:"mode"`       // default (default branch + tags, fast-forward only) | all (every branch & tag, forced)
	Visibility string `json:"visibility"` // public | private | unknown
	Enabled    bool   `json:"enabled"`
	HasToken   bool   `json:"has_token"`
	LastStatus string `json:"last_status"` // ok | error | ""
	LastError  string `json:"last_error"`
	LastAt     int64  `json:"last_at"`
	token      string
}

func repoKey(projectSlug, name string, primary bool) string {
	if primary {
		return projectSlug
	}
	return projectSlug + "/" + name
}

// validRepoKey accepts "project" or "project/repo".
func validRepoKey(k string) bool {
	parts := strings.Split(k, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !slugRe.MatchString(p) {
			return false
		}
	}
	return true
}

// ensurePrimaryRepos creates the repos row for projects made before multi-repo support.
func (s *Server) ensurePrimaryRepos() {
	s.db.Exec(`INSERT INTO repos(project_id,name,default_branch,is_primary,created_at)
		SELECT p.id, p.slug, p.default_branch, 1, p.created_at FROM projects p
		WHERE NOT EXISTS (SELECT 1 FROM repos r WHERE r.project_id=p.id AND r.is_primary=1)`)
}

func (s *Server) projectRepos(p *Project) []*Repo {
	rows, err := s.db.Query(`SELECT id,name,description,default_branch,is_primary FROM repos WHERE project_id=? ORDER BY is_primary DESC, name`, p.ID)
	if err != nil {
		return nil
	}
	var out []*Repo
	for rows.Next() {
		r := &Repo{ProjectID: p.ID}
		rows.Scan(&r.ID, &r.Name, &r.Description, &r.DefaultBranch, &r.Primary)
		if r.Primary { // the project's settings own the primary repo's default branch
			r.DefaultBranch = p.DefaultBranch
		}
		r.Key = repoKey(p.Slug, r.Name, r.Primary)
		r.CloneURL = "/git/" + r.Key + ".git"
		out = append(out, r)
	}
	rows.Close()
	if len(out) == 0 { // should not happen after ensurePrimaryRepos; be safe
		out = []*Repo{{ProjectID: p.ID, Name: p.Slug, Key: p.Slug, DefaultBranch: p.DefaultBranch, Primary: true, CloneURL: "/git/" + p.Slug + ".git"}}
	}
	for _, r := range out {
		r.Mirrors = s.repoMirrors(r.ID)
	}
	return out
}

func (s *Server) repoByName(p *Project, name string) *Repo {
	for _, r := range s.projectRepos(p) {
		if r.Name == name || (name == "" && r.Primary) {
			return r
		}
	}
	return nil
}

func (s *Server) repoMirrors(repoID int64) []*Mirror {
	out := []*Mirror{}
	rows, err := s.db.Query(`SELECT id,repo_id,url,token,mode,visibility,enabled,last_status,last_error,last_at FROM mirrors WHERE repo_id=? ORDER BY id`, repoID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		m := &Mirror{}
		rows.Scan(&m.ID, &m.RepoID, &m.URL, &m.token, &m.Mode, &m.Visibility, &m.Enabled, &m.LastStatus, &m.LastError, &m.LastAt)
		m.HasToken = m.token != ""
		out = append(out, m)
	}
	return out
}

// repoFromReq resolves {slug} plus ?repo=<name> (default: primary).
func (s *Server) repoFromReq(w http.ResponseWriter, r *http.Request) (*Project, *Repo) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return nil, nil
	}
	name := firstNonEmpty(r.PathValue("repo"), r.URL.Query().Get("repo"))
	rp := s.repoByName(p, name)
	if rp == nil {
		httpErr(w, 404, "no such repository")
		return nil, nil
	}
	return p, rp
}

// ---- API ---------------------------------------------------------------------------

func (s *Server) apiRepos(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	writeJSON(w, s.projectRepos(p))
}

func (s *Server) apiCreateRepo(w http.ResponseWriter, r *http.Request) {
	p := s.projectFromReq(w, r)
	if p == nil {
		return
	}
	var in struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
		ImportURL     string `json:"import_url"`
		ImportToken   string `json:"import_token"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if !slugRe.MatchString(in.Name) || in.Name == p.Slug {
		httpErr(w, 400, "invalid repository name (lowercase letters, digits, . _ -; not the project slug)")
		return
	}
	if s.repoByName(p, in.Name) != nil {
		httpErr(w, 409, "a repository with that name already exists")
		return
	}
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	key := repoKey(p.Slug, in.Name, false)
	if err := s.createRepoAuth(key, in.Name, in.Description, in.DefaultBranch, strings.TrimSpace(in.ImportURL), firstNonEmpty(in.ImportToken, s.setting("github_token"))); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	s.db.Exec(`INSERT INTO repos(project_id,name,description,default_branch,is_primary,created_at) VALUES(?,?,?,?,0,?)`,
		p.ID, in.Name, in.Description, in.DefaultBranch, now())
	s.emit(event{Type: "repos", ProjectID: p.ID})
	writeJSON(w, s.repoByName(p, in.Name))
}

func (s *Server) apiUpdateRepo(w http.ResponseWriter, r *http.Request) {
	p, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	var in struct {
		Description   *string `json:"description"`
		DefaultBranch *string `json:"default_branch"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if in.Description != nil {
		s.db.Exec(`UPDATE repos SET description=? WHERE id=?`, *in.Description, rp.ID)
	}
	if in.DefaultBranch != nil && *in.DefaultBranch != "" {
		if rp.Primary {
			s.db.Exec(`UPDATE projects SET default_branch=? WHERE id=?`, *in.DefaultBranch, p.ID)
		}
		s.db.Exec(`UPDATE repos SET default_branch=? WHERE id=?`, *in.DefaultBranch, rp.ID)
		s.gitStr(rp.Key, "symbolic-ref", "HEAD", "refs/heads/"+*in.DefaultBranch)
	}
	s.emit(event{Type: "repos", ProjectID: p.ID})
	w.WriteHeader(204)
}

func (s *Server) apiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	p, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	if rp.Primary {
		httpErr(w, 400, "the primary repository can't be removed (delete the project instead)")
		return
	}
	s.db.Exec(`DELETE FROM mirrors WHERE repo_id=?`, rp.ID)
	s.db.Exec(`DELETE FROM repos WHERE id=?`, rp.ID)
	trash := filepath.Join(s.DataDir, "trash", fmt.Sprintf("%s-%s-%d", p.Slug, rp.Name, time.Now().Unix()))
	os.MkdirAll(trash, 0o755)
	os.Rename(s.repoPath(rp.Key), filepath.Join(trash, "repo.git"))
	os.Rename(filepath.Join(s.DataDir, "lfs", rp.Key), filepath.Join(trash, "lfs"))
	s.emit(event{Type: "repos", ProjectID: p.ID})
	w.WriteHeader(204)
}

var ghRe = regexp.MustCompile(`^(?:https://github\.com/)?([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

func (s *Server) apiCreateMirror(w http.ResponseWriter, r *http.Request) {
	p, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	var in struct {
		URL     string `json:"url"`   // https URL, or "owner/name" for GitHub
		Token   string `json:"token"` // optional; default: the server-wide GitHub token
		Mode    string `json:"mode"`
		Create  bool   `json:"create"`  // create the GitHub repo if it doesn't exist
		Private bool   `json:"private"` // visibility when creating
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	u := strings.TrimSpace(in.URL)
	if !strings.Contains(u, "://") || strings.HasPrefix(u, "https://github.com/") { // "owner/name" or a GitHub URL
		m := ghRe.FindStringSubmatch(u)
		if m == nil {
			httpErr(w, 400, "use owner/name or https://github.com/owner/name")
			return
		}
		u = "https://github.com/" + m[1] + "/" + m[2] + ".git"
	}
	pu, err := url.Parse(u)
	if err != nil || pu.Scheme != "https" || pu.User != nil {
		httpErr(w, 400, "mirror URL must be https://… (or owner/name for GitHub) without credentials in it — put the token in the token field")
		return
	}
	if in.Mode != "all" {
		in.Mode = "default"
	}
	tok := firstNonEmpty(in.Token, s.setting("github_token"))
	if in.Create {
		if err := ensureGitHubRepo(r.Context(), u, tok, in.Private, rp.Description); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
	}
	res, err := s.db.Exec(`INSERT INTO mirrors(repo_id,url,token,mode,created_at) VALUES(?,?,?,?,?)`, rp.ID, u, in.Token, in.Mode, now())
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.emit(event{Type: "repos", ProjectID: p.ID})
	go s.syncMirror(id)
	w.WriteHeader(204)
}

func (s *Server) mirrorOf(r *http.Request, rp *Repo) *Mirror {
	id, _ := strconv.ParseInt(r.PathValue("mid"), 10, 64)
	for _, m := range rp.Mirrors {
		if m.ID == id {
			return m
		}
	}
	return nil
}

func (s *Server) apiUpdateMirror(w http.ResponseWriter, r *http.Request) {
	p, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	m := s.mirrorOf(r, rp)
	if m == nil {
		httpErr(w, 404, "no such mirror")
		return
	}
	var in struct {
		Mode    *string `json:"mode"`
		Enabled *bool   `json:"enabled"`
		Token   *string `json:"token"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if in.Mode != nil && (*in.Mode == "all" || *in.Mode == "default") {
		s.db.Exec(`UPDATE mirrors SET mode=? WHERE id=?`, *in.Mode, m.ID)
	}
	if in.Enabled != nil {
		s.db.Exec(`UPDATE mirrors SET enabled=? WHERE id=?`, *in.Enabled, m.ID)
	}
	if in.Token != nil {
		s.db.Exec(`UPDATE mirrors SET token=? WHERE id=?`, *in.Token, m.ID)
	}
	s.emit(event{Type: "repos", ProjectID: p.ID})
	go s.syncMirror(m.ID)
	w.WriteHeader(204)
}

func (s *Server) apiDeleteMirror(w http.ResponseWriter, r *http.Request) {
	p, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	if m := s.mirrorOf(r, rp); m != nil {
		s.db.Exec(`DELETE FROM mirrors WHERE id=?`, m.ID)
	}
	s.emit(event{Type: "repos", ProjectID: p.ID})
	w.WriteHeader(204)
}

func (s *Server) apiSyncMirror(w http.ResponseWriter, r *http.Request) {
	_, rp := s.repoFromReq(w, r)
	if rp == nil {
		return
	}
	m := s.mirrorOf(r, rp)
	if m == nil {
		httpErr(w, 404, "no such mirror")
		return
	}
	s.syncMirror(m.ID)
	w.WriteHeader(204)
}

// apiGitHubSettings reads/sets the server-wide GitHub token used for mirrors.
func (s *Server) apiGitHubSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		var in struct {
			Token string `json:"token"`
		}
		if err := readJSON(r, &in); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		s.setSetting("github_token", strings.TrimSpace(in.Token))
	}
	tok := s.setting("github_token")
	out := map[string]any{"has_token": tok != ""}
	if tok != "" {
		login, err := gitHubLogin(r.Context(), tok)
		out["login"], out["error"] = login, errStr(err)
	}
	writeJSON(w, out)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- mirroring ---------------------------------------------------------------------

var (
	mirrorMu    sync.Mutex
	mirrorLocks = map[int64]*sync.Mutex{}
)

func mirrorLock(id int64) *sync.Mutex {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	if mirrorLocks[id] == nil {
		mirrorLocks[id] = &sync.Mutex{}
	}
	return mirrorLocks[id]
}

// mirrorsChanged pushes every enabled mirror of a repo (after a push/merge).
func (s *Server) mirrorsChanged(key string) {
	rows, err := s.db.Query(`SELECT m.id FROM mirrors m JOIN repos r ON r.id=m.repo_id JOIN projects p ON p.id=r.project_id
		WHERE m.enabled=1 AND (CASE WHEN r.is_primary=1 THEN p.slug ELSE p.slug || '/' || r.name END)=?`, key)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		go s.syncMirror(id)
	}
}

// mirrorLoop retries failed mirrors and catches anything missed.
func (s *Server) mirrorLoop() {
	t := time.NewTicker(10 * time.Minute)
	for range t.C {
		rows, err := s.db.Query(`SELECT id FROM mirrors WHERE enabled=1`)
		if err != nil {
			continue
		}
		var ids []int64
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			s.syncMirror(id)
		}
	}
}

func (s *Server) syncMirror(id int64) {
	l := mirrorLock(id)
	l.Lock()
	defer l.Unlock()
	var m Mirror
	var repoID, pid int64
	var name, slug, branch, pBranch string
	var primary bool
	err := s.db.QueryRow(`SELECT m.url,m.token,m.mode,m.enabled,m.visibility,m.last_at,r.id,r.name,r.is_primary,r.default_branch,p.id,p.slug,p.default_branch
		FROM mirrors m JOIN repos r ON r.id=m.repo_id JOIN projects p ON p.id=r.project_id WHERE m.id=?`, id).
		Scan(&m.URL, &m.token, &m.Mode, &m.Enabled, &m.Visibility, &m.LastAt, &repoID, &name, &primary, &branch, &pid, &slug, &pBranch)
	if err != nil || !m.Enabled {
		return
	}
	if primary {
		branch = pBranch
	}
	key := repoKey(slug, name, primary)
	tok := firstNonEmpty(m.token, s.setting("github_token"))

	// Refresh the visibility now and then (unauthenticated: 200 = public).
	if m.Visibility == "unknown" || time.Since(time.UnixMilli(m.LastAt)) > time.Hour {
		if v := gitHubVisibility(m.URL); v != "" {
			s.db.Exec(`UPDATE mirrors SET visibility=? WHERE id=?`, v, id)
		}
	}

	args := []string{"--git-dir", s.repoPath(key)}
	if tok != "" {
		args = append(args, "-c", "http.extraHeader=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)))
	}
	args = append(args, "push", "--porcelain")
	if m.Mode == "all" {
		args = append(args, "--force", "--prune", m.URL, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*")
	} else {
		args = append(args, m.URL, "refs/heads/"+branch+":refs/heads/"+branch, "refs/tags/*:refs/tags/*")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	out, err := cmd.CombinedOutput()
	status, msg := "ok", ""
	if err != nil {
		status, msg = "error", mirrorError(string(out), tok)
		log.Printf("mirror %s → %s: %s", key, redactURL(m.URL), msg)
	}
	s.db.Exec(`UPDATE mirrors SET last_status=?, last_error=?, last_at=? WHERE id=?`, status, msg, now(), id)
	s.emit(event{Type: "repos", ProjectID: pid})
}

func mirrorError(out, tok string) string {
	if tok != "" {
		out = strings.ReplaceAll(out, tok, "***")
	}
	switch {
	case strings.Contains(out, "non-fast-forward") || strings.Contains(out, "[rejected]") || strings.Contains(out, "fetch first"):
		return "rejected: the mirror has commits that are not on this server. Mirrors are one-way (MultiClaude → GitHub); switch the mirror to \"all branches (forced)\" or reset the GitHub repo."
	case strings.Contains(out, "Authentication failed") || strings.Contains(out, "403") || strings.Contains(out, "could not read Username"):
		return "authentication failed: set a GitHub token with Contents: read & write for this repository"
	case strings.Contains(out, "not found"):
		return "repository not found (create it on GitHub, or check the token can access it)"
	}
	return lastLines(strings.TrimSpace(out), 6)
}

func lastLines(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func redactURL(u string) string {
	if p, err := url.Parse(u); err == nil {
		p.User = nil
		return p.String()
	}
	return u
}

// ---- GitHub API --------------------------------------------------------------------

var ghHTTP = &http.Client{Timeout: 20 * time.Second}

func ghOwnerRepo(u string) (string, string, bool) {
	m := ghRe.FindStringSubmatch(u)
	if m == nil || !strings.HasPrefix(u, "https://github.com/") {
		return "", "", false
	}
	return m[1], m[2], true
}

func gitHubAPI(ctx context.Context, method, path, tok string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := ghHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct{ Message string }
		json.Unmarshal(b, &e)
		return resp.StatusCode, fmt.Errorf("GitHub API %s %s: HTTP %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil {
		json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

// gitHubVisibility: an anonymous request succeeds only for public repos.
func gitHubVisibility(u string) string {
	o, n, ok := ghOwnerRepo(u)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, err := gitHubAPI(ctx, "GET", "/repos/"+o+"/"+n, "", nil, nil)
	switch {
	case err == nil:
		return "public"
	case code == 404:
		return "private"
	}
	return ""
}

func gitHubLogin(ctx context.Context, tok string) (string, error) {
	var u struct{ Login string }
	_, err := gitHubAPI(ctx, "GET", "/user", tok, nil, &u)
	return u.Login, err
}

// ensureGitHubRepo creates owner/name (as an org repo if owner isn't the token's user).
func ensureGitHubRepo(ctx context.Context, u, tok string, private bool, description string) error {
	o, n, ok := ghOwnerRepo(u)
	if !ok {
		return errors.New("creating repositories is only supported on github.com")
	}
	if tok == "" {
		return errors.New("a GitHub token is needed to create the repository (Users & account → GitHub, or the token field)")
	}
	if code, err := gitHubAPI(ctx, "GET", "/repos/"+o+"/"+n, tok, nil, nil); err == nil {
		return nil // exists
	} else if code != 404 {
		return err
	}
	login, err := gitHubLogin(ctx, tok)
	if err != nil {
		return err
	}
	body := map[string]any{"name": n, "private": private, "description": description, "has_issues": false, "has_wiki": false, "auto_init": false}
	path := "/orgs/" + o + "/repos"
	if strings.EqualFold(login, o) {
		path = "/user/repos"
	}
	_, err = gitHubAPI(ctx, "POST", path, tok, body, nil)
	return err
}

// ---- repo creation with an authenticated import ----------------------------------------

// createRepoAuth is createRepo with an optional token for importing a private repo.
func (s *Server) createRepoAuth(key, name, description, branch, importURL, tok string) error {
	if importURL != "" && tok != "" && strings.HasPrefix(importURL, "https://github.com/") {
		hdr := "http.extraHeader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
		return s.createRepoWith(key, name, description, branch, importURL, []string{"-c", hdr})
	}
	return s.createRepoWith(key, name, description, branch, importURL, nil)
}
