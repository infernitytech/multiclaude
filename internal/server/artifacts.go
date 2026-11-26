package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Artifact store: per-project files outside git — builds (as archives),
// datasets, model checkpoints, large generated files. Agents use the
// `multiclaude artifact` command in their shell (it authenticates with the
// machine token); humans use the Artifacts tab. Files are streamed to disk,
// so size is limited only by MC_ARTIFACT_MAX_GB (default 100) and free space.
// A name is unique per project: uploading the same name replaces the file.

const artifactSchema = `
CREATE TABLE IF NOT EXISTS artifacts(
  id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL,
  name TEXT NOT NULL,
  size INTEGER NOT NULL,
  sha256 TEXT NOT NULL,
  content_type TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  issue_id INTEGER,
  run_id TEXT NOT NULL DEFAULT '',
  uploaded_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  downloads INTEGER NOT NULL DEFAULT 0,
  UNIQUE(project_id, name)
);
CREATE INDEX IF NOT EXISTS artifacts_issue ON artifacts(issue_id);
`

type Artifact struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"project_id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	Description string `json:"description"`
	IssueID     *int64 `json:"issue_id"`
	IssueNumber int    `json:"issue_number,omitempty"`
	RunID       string `json:"run_id"`
	UploadedBy  string `json:"uploaded_by"`
	CreatedAt   int64  `json:"created_at"`
	Downloads   int    `json:"downloads"`
}

const artifactSelect = `SELECT a.id,a.project_id,a.name,a.size,a.sha256,a.content_type,a.description,a.issue_id,COALESCE(i.number,0),a.run_id,a.uploaded_by,a.created_at,a.downloads
  FROM artifacts a LEFT JOIN issues i ON i.id=a.issue_id `

func scanArtifact(r interface{ Scan(...any) error }) (*Artifact, error) {
	a := &Artifact{}
	var issue sql.NullInt64
	err := r.Scan(&a.ID, &a.ProjectID, &a.Name, &a.Size, &a.SHA256, &a.ContentType, &a.Description, &issue, &a.IssueNumber, &a.RunID, &a.UploadedBy, &a.CreatedAt, &a.Downloads)
	if issue.Valid {
		a.IssueID = &issue.Int64
	}
	return a, err
}

func (s *Server) artifactDir(pid int64) string {
	return filepath.Join(s.DataDir, "artifacts", strconv.FormatInt(pid, 10))
}

func (s *Server) artifactFile(a *Artifact) string {
	return filepath.Join(s.artifactDir(a.ProjectID), strconv.FormatInt(a.ID, 10))
}

// cleanArtifactName normalises "builds/game-1.2-linux.tar.gz"; no "..", no absolute paths.
func cleanArtifactName(n string) (string, error) {
	n = strings.Trim(strings.ReplaceAll(n, "\\", "/"), "/ ")
	c := path.Clean(n)
	if n == "" || c == "." || strings.HasPrefix(c, "../") || c == ".." || len(c) > 300 {
		return "", errors.New("invalid artifact name")
	}
	for _, r := range c {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("invalid artifact name")
		}
	}
	return c, nil
}

// artifactAccess: users see every project; a machine only the projects it is assigned to.
func (s *Server) artifactProject(w http.ResponseWriter, r *http.Request) *Project {
	p := s.projectFromReq(w, r)
	if p == nil {
		return nil
	}
	if node := nodeFrom(r); node != "" {
		ok := false
		for _, id := range s.nodeProjects(node) {
			ok = ok || id == p.ID
		}
		if !ok {
			httpErr(w, 403, "this machine is not assigned to project "+p.Slug)
			return nil
		}
	}
	return p
}

func maxArtifactBytes() int64 {
	gb, _ := strconv.ParseFloat(os.Getenv("MC_ARTIFACT_MAX_GB"), 64)
	if gb <= 0 {
		gb = 100
	}
	return int64(gb * (1 << 30))
}

// PUT /api/projects/{slug}/artifacts/{name...}   body = file content
// Query/headers: description (X-Description), run (X-MC-Run: links the agent's issue).
func (s *Server) apiPutArtifact(w http.ResponseWriter, r *http.Request) {
	p := s.artifactProject(w, r)
	if p == nil {
		return
	}
	name, err := cleanArtifactName(r.PathValue("name"))
	if err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	limit := maxArtifactBytes()
	if r.ContentLength > limit {
		httpErr(w, 413, fmt.Sprintf("artifact larger than the limit of %d GB (MC_ARTIFACT_MAX_GB)", limit>>30))
		return
	}
	by, issueID, runID := "human", (*int64)(nil), ""
	if u := userFrom(r); u != nil {
		by = u.Username
	}
	if id := firstNonEmpty(r.Header.Get("X-MC-Run"), r.URL.Query().Get("run")); id != "" {
		if run, err := s.runByID(id); err == nil && run.ProjectID == p.ID && (nodeFrom(r) == "" || run.NodeID == nodeFrom(r)) {
			by, issueID, runID = run.handle(), run.IssueID, run.ID
		}
	} else if node := nodeFrom(r); node != "" {
		if n, err := s.node(node); err == nil {
			by = n.Name
		}
	}
	if v := r.URL.Query().Get("issue"); v != "" && issueID == nil {
		if n, err := strconv.Atoi(strings.TrimPrefix(v, "#")); err == nil {
			if i, err := s.issueByNumber(p.ID, n); err == nil {
				issueID = &i.ID
			}
		}
	}
	desc := firstNonEmpty(r.Header.Get("X-Description"), r.URL.Query().Get("description"))
	ctype := r.Header.Get("Content-Type")
	if ctype == "" || ctype == "application/x-www-form-urlencoded" || ctype == "application/octet-stream" {
		ctype = mime.TypeByExtension(path.Ext(name))
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			ctype = "application/gzip"
		}
	}

	dir := s.artifactDir(p.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, limit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		httpErr(w, 400, "upload failed: "+err.Error())
		return
	}
	if r.ContentLength > 0 && n != r.ContentLength {
		httpErr(w, 400, "upload incomplete")
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want := r.Header.Get("X-SHA256"); want != "" && !strings.EqualFold(want, sum) {
		httpErr(w, 400, "checksum mismatch")
		return
	}

	// Insert or replace (same name), then move the file into place under the row id.
	var id int64
	replaced := s.db.QueryRow(`SELECT id FROM artifacts WHERE project_id=? AND name=?`, p.ID, name).Scan(&id) == nil
	if replaced {
		_, err = s.db.Exec(`UPDATE artifacts SET size=?, sha256=?, content_type=?, description=?, issue_id=?, run_id=?, uploaded_by=?, created_at=? WHERE id=?`,
			n, sum, ctype, desc, issueID, runID, by, now(), id)
	} else {
		var res sql.Result
		res, err = s.db.Exec(`INSERT INTO artifacts(project_id,name,size,sha256,content_type,description,issue_id,run_id,uploaded_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			p.ID, name, n, sum, ctype, desc, issueID, runID, by, now())
		if err == nil {
			id, _ = res.LastInsertId()
		}
	}
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, strconv.FormatInt(id, 10))); err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	a, _ := scanArtifact(s.db.QueryRow(artifactSelect+`WHERE a.id=?`, id))
	if issueID != nil && runID != "" {
		verb := "uploaded"
		if replaced {
			verb = "replaced"
		}
		note := fmt.Sprintf("📦 %s artifact `%s` (%s)", verb, name, humanBytes(n))
		if desc != "" {
			note += " — " + desc
		}
		s.addComment(*issueID, by, note)
		s.issueChanged(p.ID)
	}
	s.emit(event{Type: "artifacts", ProjectID: p.ID})
	writeJSON(w, a)
}

// GET /api/projects/{slug}/artifacts[?q=&prefix=&issue=]
func (s *Server) apiArtifacts(w http.ResponseWriter, r *http.Request) {
	p := s.artifactProject(w, r)
	if p == nil {
		return
	}
	list, total, size := s.listArtifacts(p.ID, r.URL.Query().Get("q"), r.URL.Query().Get("prefix"), r.URL.Query().Get("issue"), 500)
	writeJSON(w, map[string]any{"items": list, "total": total, "total_size": size, "max_bytes": maxArtifactBytes()})
}

func (s *Server) listArtifacts(pid int64, q, prefix, issue string, limit int) ([]*Artifact, int, int64) {
	where := []string{"a.project_id=?"}
	args := []any{pid}
	if prefix = strings.Trim(prefix, "/ "); prefix != "" {
		where = append(where, "a.name LIKE ? ESCAPE '\\'")
		args = append(args, likeEscape(prefix)+"%")
	}
	for _, word := range strings.Fields(q) {
		where = append(where, "(a.name LIKE ? ESCAPE '\\' OR a.description LIKE ? ESCAPE '\\' OR a.uploaded_by LIKE ? ESCAPE '\\')")
		l := "%" + likeEscape(word) + "%"
		args = append(args, l, l, l)
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(issue, "#")); err == nil {
		where = append(where, "i.number=?")
		args = append(args, n)
	}
	w := strings.Join(where, " AND ")
	var total int
	var size int64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(a.size),0) FROM artifacts a LEFT JOIN issues i ON i.id=a.issue_id WHERE `+w, args...).Scan(&total, &size)
	rows, err := s.db.Query(artifactSelect+`WHERE `+w+` ORDER BY a.created_at DESC LIMIT ?`, append(args, limit)...)
	out := []*Artifact{}
	if err != nil {
		return out, total, size
	}
	defer rows.Close()
	for rows.Next() {
		if a, err := scanArtifact(rows); err == nil {
			out = append(out, a)
		}
	}
	return out, total, size
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (s *Server) artifactFromReq(w http.ResponseWriter, r *http.Request) (*Project, *Artifact) {
	p := s.artifactProject(w, r)
	if p == nil {
		return nil, nil
	}
	name, err := cleanArtifactName(r.PathValue("name"))
	if err != nil {
		httpErr(w, 400, err.Error())
		return nil, nil
	}
	a, err := scanArtifact(s.db.QueryRow(artifactSelect+`WHERE a.project_id=? AND a.name=?`, p.ID, name))
	if err != nil {
		httpErr(w, 404, "artifact not found: "+name)
		return nil, nil
	}
	return p, a
}

// GET /api/projects/{slug}/artifacts/{name...} — download (supports Range/resume).
func (s *Server) apiGetArtifact(w http.ResponseWriter, r *http.Request) {
	_, a := s.artifactFromReq(w, r)
	if a == nil {
		return
	}
	f, err := os.Open(s.artifactFile(a))
	if err != nil {
		httpErr(w, 410, "artifact file is missing on the server")
		return
	}
	defer f.Close()
	if r.Method == "GET" && r.Header.Get("Range") == "" {
		s.db.Exec(`UPDATE artifacts SET downloads=downloads+1 WHERE id=?`, a.ID)
	}
	if a.ContentType != "" {
		w.Header().Set("Content-Type", a.ContentType)
	}
	w.Header().Set("X-SHA256", a.SHA256)
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(a.Name)}))
	http.ServeContent(w, r, "", time.UnixMilli(a.CreatedAt), f)
}

// DELETE /api/projects/{slug}/artifacts/{name...}
func (s *Server) apiDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	p, a := s.artifactFromReq(w, r)
	if a == nil {
		return
	}
	s.db.Exec(`DELETE FROM artifacts WHERE id=?`, a.ID)
	os.Remove(s.artifactFile(a))
	s.emit(event{Type: "artifacts", ProjectID: p.ID})
	w.WriteHeader(204)
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}
