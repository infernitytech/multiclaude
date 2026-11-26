package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Minimal Git LFS server: batch API + basic transfer adapter + no-op locks.
// Objects live in <data>/lfs/<slug>/<aa>/<bb>/<oid>.

var oidRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

const lfsMime = "application/vnd.git-lfs+json"

func (s *Server) lfsPath(slug, oid string) string {
	// "project/repo" → lfs/project~repo (keeps it apart from the primary's aa/bb dirs)
	return filepath.Join(s.DataDir, "lfs", strings.ReplaceAll(slug, "/", "~"), oid[0:2], oid[2:4], oid)
}

func lfsJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", lfsMime)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

func (s *Server) handleLFS(w http.ResponseWriter, r *http.Request, slug, sub string) {
	switch {
	case sub == "objects/batch" && r.Method == http.MethodPost:
		s.lfsBatch(w, r, slug)
	case strings.HasPrefix(sub, "objects/"):
		oid := strings.TrimPrefix(sub, "objects/")
		if !oidRe.MatchString(oid) {
			lfsJSON(w, 404, map[string]string{"message": "invalid oid"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			f, err := os.Open(s.lfsPath(slug, oid))
			if err != nil {
				lfsJSON(w, 404, map[string]string{"message": "object not found"})
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, oid, fileModTime(f), f)
		case http.MethodPut:
			s.lfsUpload(w, r, slug, oid)
		default:
			http.Error(w, "method not allowed", 405)
		}
	case sub == "locks/verify":
		lfsJSON(w, 200, map[string]any{"ours": []any{}, "theirs": []any{}})
	case sub == "locks" && r.Method == http.MethodGet:
		lfsJSON(w, 200, map[string]any{"locks": []any{}})
	default:
		lfsJSON(w, 404, map[string]string{"message": "not supported"})
	}
}

type lfsObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request, slug string) {
	var req struct {
		Operation string      `json:"operation"`
		Objects   []lfsObject `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		lfsJSON(w, 422, map[string]string{"message": err.Error()})
		return
	}
	href := fmt.Sprintf("%s/git/%s.git/info/lfs/objects/", baseURL(r), slug)
	// Transfers must carry the same credentials as the batch request.
	var hdr map[string]string
	if a := r.Header.Get("Authorization"); a != "" {
		hdr = map[string]string{"Authorization": a}
	}
	action := func(h string) map[string]any {
		a := map[string]any{"href": h}
		if hdr != nil {
			a["header"] = hdr
		}
		return a
	}
	out := make([]map[string]any, 0, len(req.Objects))
	for _, o := range req.Objects {
		obj := map[string]any{"oid": o.OID, "size": o.Size, "authenticated": true}
		if !oidRe.MatchString(o.OID) {
			obj["error"] = map[string]any{"code": 422, "message": "invalid oid"}
			out = append(out, obj)
			continue
		}
		st, err := os.Stat(s.lfsPath(slug, o.OID))
		exists := err == nil && st.Size() == o.Size
		switch req.Operation {
		case "upload":
			if !exists {
				obj["actions"] = map[string]any{"upload": action(href + o.OID)}
			}
		default:
			if exists {
				obj["actions"] = map[string]any{"download": action(href + o.OID)}
			} else {
				obj["error"] = map[string]any{"code": 404, "message": "object not found"}
			}
		}
		out = append(out, obj)
	}
	lfsJSON(w, 200, map[string]any{"transfer": "basic", "objects": out, "hash_algo": "sha256"})
}

func (s *Server) lfsUpload(w http.ResponseWriter, r *http.Request, slug, oid string) {
	dst := s.lfsPath(slug, oid)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r.Body); err != nil {
		tmp.Close()
		http.Error(w, err.Error(), 500)
		return
	}
	tmp.Close()
	if hex.EncodeToString(h.Sum(nil)) != oid {
		lfsJSON(w, 422, map[string]string{"message": "sha256 mismatch"})
		return
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(200)
}
