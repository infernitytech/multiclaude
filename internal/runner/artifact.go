package runner

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const artifactUsage = `multiclaude artifact — the project's artifact store (builds, datasets, checkpoints, large files)

  multiclaude artifact put <file|dir> [name] [-m "description"] [--issue N]
        upload; a directory is packed as <name>.tar.gz. The same name replaces the old file.
  multiclaude artifact get <name> [dest] [-x]
        download (resumes, verifies the checksum); -x extracts a .tar.gz/.tgz into dest (a directory)
  multiclaude artifact ls [prefix]
  multiclaude artifact rm <name>

Inside agent sessions server, project and credentials come from the environment.
Elsewhere: --server URL --project SLUG (credentials of the enrolled machine are used).
`

type artifactClient struct {
	server, project, token, run string
	http                        *http.Client
}

// ArtifactCLI implements `multiclaude artifact …`; returns the exit code.
func ArtifactCLI(args []string) int {
	c := &artifactClient{server: os.Getenv("MULTICLAUDE_SERVER"), project: os.Getenv("MULTICLAUDE_PROJECT"),
		token: os.Getenv("MC_TOKEN"), run: os.Getenv("MULTICLAUDE_RUN_ID"), http: &http.Client{}}
	var pos []string
	desc, issue, extract := "", "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "-m", "--message", "--description", "-d":
			desc = next()
		case "--issue":
			issue = next()
		case "-x", "--extract":
			extract = true
		case "--server":
			c.server = next()
		case "--project", "-p":
			c.project = next()
		case "-h", "--help", "help":
			fmt.Print(artifactUsage)
			return 0
		default:
			pos = append(pos, a)
		}
	}
	if c.token == "" { // a human on an enrolled machine
		cr := loadCredentials(defaultWorkDir(os.Getenv("MC_WORKDIR")))
		c.token = cr.Token
		if c.server == "" {
			c.server = cr.Server
		}
	}
	c.server = strings.TrimRight(c.server, "/")
	if len(pos) == 0 || c.server == "" || c.project == "" {
		fmt.Fprint(os.Stderr, artifactUsage)
		if len(pos) > 0 {
			fmt.Fprintln(os.Stderr, "\nerror: server and project unknown (use --server and --project)")
		}
		return 2
	}
	var err error
	switch pos[0] {
	case "put", "upload", "push":
		if len(pos) < 2 {
			err = errors.New("usage: multiclaude artifact put <file|dir> [name]")
			break
		}
		name := ""
		if len(pos) > 2 {
			name = pos[2]
		}
		err = c.put(pos[1], name, desc, issue)
	case "get", "download", "pull":
		if len(pos) < 2 {
			err = errors.New("usage: multiclaude artifact get <name> [dest]")
			break
		}
		dest := ""
		if len(pos) > 2 {
			dest = pos[2]
		}
		err = c.get(pos[1], dest, extract)
	case "ls", "list":
		prefix := ""
		if len(pos) > 1 {
			prefix = pos[1]
		}
		err = c.ls(prefix)
	case "rm", "delete":
		if len(pos) < 2 {
			err = errors.New("usage: multiclaude artifact rm <name>")
			break
		}
		err = c.rm(pos[1])
	default:
		fmt.Fprint(os.Stderr, artifactUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func (c *artifactClient) url(name string) string {
	u := c.server + "/api/projects/" + url.PathEscape(c.project) + "/artifacts"
	if name != "" {
		var segs []string
		for _, s := range strings.Split(strings.Trim(name, "/"), "/") {
			segs = append(segs, url.PathEscape(s))
		}
		u += "/" + strings.Join(segs, "/")
	}
	return u
}

func (c *artifactClient) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.run != "" {
		req.Header.Set("X-MC-Run", c.run)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct {
			Error string `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return resp, &httpError{resp.StatusCode, e.Error}
	}
	return resp, nil
}

// retryable: the server is down or restarting — worth waiting for.
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.code == 502 || he.code == 503 || he.code == 504
	}
	return err != nil
}

func withRetry(what string, f func() error) error {
	wait, deadline := 2*time.Second, time.Now().Add(15*time.Minute)
	for {
		err := f()
		if err == nil || !retryable(err) || time.Now().After(deadline) {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %v — retrying in %s\n", what, err, wait)
		time.Sleep(wait)
		wait = min(wait*2, 30*time.Second)
	}
}

func (c *artifactClient) put(src, name, desc, issue string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	file := src
	if st.IsDir() {
		base := filepath.Base(filepath.Clean(src))
		if name == "" {
			name = base + ".tar.gz"
		} else if !strings.HasSuffix(name, ".tar.gz") && !strings.HasSuffix(name, ".tgz") {
			name += ".tar.gz"
		}
		tmp, err := os.CreateTemp("", "mc-artifact-*.tar.gz")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		fmt.Fprintf(os.Stderr, "packing %s …\n", src)
		if err := tarGz(tmp, src, base); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		file = tmp.Name()
	} else if name == "" {
		name = filepath.Base(src)
	} else if strings.HasSuffix(name, "/") {
		name += filepath.Base(src)
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	q := url.Values{}
	if desc != "" {
		q.Set("description", desc)
	}
	if issue != "" {
		q.Set("issue", issue)
	}
	var out struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	err = withRetry("upload", func() error {
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
		body := &progress{r: f, total: size, label: "uploading " + name}
		req, _ := http.NewRequest("PUT", c.url(name)+"?"+q.Encode(), body)
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("X-SHA256", sum)
		resp, err := c.do(req)
		body.done()
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(&out)
	})
	if err != nil {
		return err
	}
	fmt.Printf("uploaded %s (%s, sha256 %s)\n", out.Name, human(out.Size), sum[:12])
	return nil
}

func (c *artifactClient) get(name, dest string, extract bool) error {
	base := path.Base(strings.Trim(name, "/"))
	if extract {
		if dest == "" {
			dest = "."
		}
		tmp, err := os.CreateTemp("", "mc-artifact-*")
		if err != nil {
			return err
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		if err := c.download(name, tmp.Name()); err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		if err := untarGz(tmp.Name(), dest); err != nil {
			return fmt.Errorf("extracting %s: %w", name, err)
		}
		fmt.Printf("extracted %s into %s\n", name, dest)
		return nil
	}
	if dest == "" {
		dest = base
	} else if st, err := os.Stat(dest); err == nil && st.IsDir() {
		dest = filepath.Join(dest, base)
	}
	if err := c.download(name, dest); err != nil {
		return err
	}
	fmt.Printf("downloaded %s → %s\n", name, dest)
	return nil
}

// download resumes partial downloads and verifies the server's checksum.
func (c *artifactClient) download(name, dest string) error {
	part := dest + ".part"
	var sum string
	err := withRetry("download", func() error {
		f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		have, _ := f.Seek(0, io.SeekEnd)
		req, _ := http.NewRequest("GET", c.url(name), nil)
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := c.do(req)
		var he *httpError
		if errors.As(err, &he) && he.code == 416 { // already complete
			sum = resp.Header.Get("X-SHA256")
			return nil
		}
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		sum = resp.Header.Get("X-SHA256")
		if resp.StatusCode != http.StatusPartialContent {
			f.Truncate(0)
			f.Seek(0, 0)
			have = 0
		}
		total := have + resp.ContentLength
		_, err = io.Copy(f, &progress{r: resp.Body, total: total, n: have, label: "downloading " + name})
		return err
	})
	if err != nil {
		return err
	}
	if sum != "" {
		f, err := os.Open(part)
		if err != nil {
			return err
		}
		h := sha256.New()
		io.Copy(h, f)
		f.Close()
		if got := hex.EncodeToString(h.Sum(nil)); got != sum {
			os.Remove(part)
			return fmt.Errorf("checksum mismatch (got %s, want %s) — try again", got[:12], sum[:12])
		}
	}
	return os.Rename(part, dest)
}

func (c *artifactClient) ls(prefix string) error {
	req, _ := http.NewRequest("GET", c.url("")+"?prefix="+url.QueryEscape(prefix), nil)
	var out struct {
		Items []struct {
			Name        string `json:"name"`
			Size        int64  `json:"size"`
			UploadedBy  string `json:"uploaded_by"`
			CreatedAt   int64  `json:"created_at"`
			IssueNumber int    `json:"issue_number"`
			Description string `json:"description"`
		} `json:"items"`
		Total     int   `json:"total"`
		TotalSize int64 `json:"total_size"`
	}
	err := withRetry("list", func() error {
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(&out)
	})
	if err != nil {
		return err
	}
	for _, a := range out.Items {
		issue := ""
		if a.IssueNumber > 0 {
			issue = " #" + strconv.Itoa(a.IssueNumber)
		}
		fmt.Printf("%-50s %10s  %s  %s%s", a.Name, human(a.Size), time.UnixMilli(a.CreatedAt).Format("2006-01-02 15:04"), a.UploadedBy, issue)
		if a.Description != "" {
			fmt.Printf("  — %s", a.Description)
		}
		fmt.Println()
	}
	fmt.Printf("%d artifact(s), %s\n", out.Total, human(out.TotalSize))
	return nil
}

func (c *artifactClient) rm(name string) error {
	return withRetry("delete", func() error {
		req, _ := http.NewRequest("DELETE", c.url(name), nil)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		fmt.Println("deleted", name)
		return nil
	})
}

// ---- helpers ----------------------------------------------------------------------

type progress struct {
	r            io.Reader
	n, total     int64
	label        string
	last         time.Time
	printedFinal bool
}

func (p *progress) Read(b []byte) (int, error) {
	k, err := p.r.Read(b)
	p.n += int64(k)
	if p.total > 1<<20 && time.Since(p.last) > 5*time.Second {
		p.last = time.Now()
		fmt.Fprintf(os.Stderr, "%s: %s / %s (%d%%)\n", p.label, human(p.n), human(p.total), p.n*100/max(p.total, 1))
	}
	return k, err
}

func (p *progress) done() {}

func human(n int64) string {
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

// tarGz packs dir as <prefix>/… (symlinks kept as links).
func tarGz(w io.Writer, dir, prefix string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			link, _ = os.Readlink(p)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// untarGz extracts a .tar.gz into dest, refusing paths that escape it.
func untarGz(file, dest string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root, _ := filepath.Abs(dest)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
				continue // don't create links pointing outside
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Symlink(hdr.Linkname, target)
		}
	}
}
