package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (s *Server) repoPath(slug string) string { return filepath.Join(s.DataDir, "repos", slug+".git") }

var gitIdentity = []string{
	"GIT_AUTHOR_NAME=MultiClaude", "GIT_AUTHOR_EMAIL=multiclaude@localhost",
	"GIT_COMMITTER_NAME=MultiClaude", "GIT_COMMITTER_EMAIL=multiclaude@localhost",
}

// git runs a git command against a bare repo.
func (s *Server) git(slug string, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", s.repoPath(slug)}, args...)...)
	cmd.Env = append(os.Environ(), gitIdentity...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (s *Server) gitStr(slug string, args ...string) (string, error) {
	b, err := s.git(slug, nil, args...)
	return strings.TrimSpace(string(b)), err
}

func configureRepo(path string) error {
	for _, kv := range [][2]string{
		{"http.receivepack", "true"},
		{"http.uploadpack", "true"},
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
		{"receive.denyNonFastForwards", "false"},
		{"core.logAllRefUpdates", "true"},
	} {
		if out, err := exec.Command("git", "--git-dir", path, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			return fmt.Errorf("git config: %v %s", err, out)
		}
	}
	return nil
}

// createRepo makes a bare repo. With importURL it mirrors an existing repo
// (including LFS objects if git-lfs is available); otherwise it seeds an
// initial commit so that every agent can branch from the default branch.
func (s *Server) createRepo(slug, name, description, branch, importURL string) error {
	return s.createRepoWith(slug, name, description, branch, importURL, nil)
}

// createRepoWith takes extra git options for the import (e.g. an auth header).
func (s *Server) createRepoWith(slug, name, description, branch, importURL string, gitOpts []string) error {
	path := s.repoPath(slug)
	if _, err := os.Stat(path); err == nil {
		return configureRepo(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if importURL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		c := exec.CommandContext(ctx, "git", append(append(append([]string{}, gitOpts...), "clone", "--bare"), importURL, path)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
		if out, err := c.CombinedOutput(); err != nil {
			os.RemoveAll(path)
			return fmt.Errorf("import failed: %v: %s", err, out)
		}
		if err := configureRepo(path); err != nil {
			return err
		}
		// The configured default branch must exist. If none was given (or it's
		// missing) adopt the imported repository's own default branch — never
		// invent a new one, or mirrors would push a stray branch upstream.
		if _, err := s.gitStr(slug, "rev-parse", "--verify", "refs/heads/"+branch); branch == "" || err != nil {
			head, err2 := s.gitStr(slug, "symbolic-ref", "--short", "HEAD")
			if err2 != nil {
				return fmt.Errorf("imported repo has no default branch")
			}
			if _, err := s.gitStr(slug, "rev-parse", "--verify", "refs/heads/"+head); err != nil {
				return fmt.Errorf("imported repo is empty")
			}
			branch = head
		}
		s.gitStr(slug, "symbolic-ref", "HEAD", "refs/heads/"+branch)
		go s.importLFS(slug, importURL, gitOpts)
		return nil
	}
	if out, err := exec.Command("git", "init", "--bare", "-b", branch, path).CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %v: %s", err, out)
	}
	if err := configureRepo(path); err != nil {
		return err
	}
	readme := fmt.Sprintf("# %s\n\n%s\n", name, description)
	attrs := "# Git LFS is enabled on this server. Track large files with e.g.\n#   git lfs track \"*.bin\"\n"
	readmeSHA, err := s.git(slug, []byte(readme), "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	attrSHA, err := s.git(slug, []byte(attrs), "hash-object", "-w", "--stdin")
	if err != nil {
		return err
	}
	tree := fmt.Sprintf("100644 blob %s\t.gitattributes\n100644 blob %s\tREADME.md\n",
		strings.TrimSpace(string(attrSHA)), strings.TrimSpace(string(readmeSHA)))
	treeSHA, err := s.git(slug, []byte(tree), "mktree")
	if err != nil {
		return err
	}
	commit, err := s.gitStr(slug, "commit-tree", strings.TrimSpace(string(treeSHA)), "-m", "Initial commit")
	if err != nil {
		return err
	}
	_, err = s.gitStr(slug, "update-ref", "refs/heads/"+branch, commit)
	return err
}

func (s *Server) importLFS(slug, url string, gitOpts []string) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		return
	}
	tmp, err := os.MkdirTemp("", "mc-lfs-")
	if err != nil {
		return
	}
	defer os.RemoveAll(tmp)
	cmd := exec.Command("git", append(append(append([]string{}, gitOpts...), "clone"), url, tmp)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	if out, err := cmd.CombinedOutput(); err != nil {
		s.logf("lfs import clone: %v %s", err, out)
		return
	}
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = tmp
		if out, err := c.CombinedOutput(); err != nil {
			s.logf("lfs import %v: %v %s", args, err, out)
		}
	}
	run(append(append([]string{}, gitOpts...), "lfs", "fetch", "--all", "origin")...)
	run("remote", "add", "mc", fmt.Sprintf("http://127.0.0.1%s/git/%s.git", s.listenPort(), slug))
	run("-c", "http.extraHeader=Authorization: Bearer "+s.internalToken, "lfs", "push", "--all", "mc")
}

// ---- smart HTTP ---------------------------------------------------------

func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	idx := strings.Index(rest, ".git")
	if idx <= 0 {
		http.NotFound(w, r)
		return
	}
	slug := rest[:idx]
	sub := rest[idx+4:]
	if !validRepoKey(slug) {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(s.repoPath(slug)); err != nil {
		http.Error(w, "repository not found", 404)
		return
	}
	if strings.HasPrefix(sub, "/info/lfs/") {
		s.handleLFS(w, r, slug, strings.TrimPrefix(sub, "/info/lfs/"))
		return
	}
	h := &cgi.Handler{
		Path: mustLookPath("git"),
		Args: []string{"http-backend"},
		Root: "/git",
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(s.DataDir, "repos"),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_HTTP_MAX_REQUEST_BUFFER=1000M",
			"REMOTE_USER=multiclaude",
		},
		InheritEnv: []string{"PATH", "HOME"},
	}
	h.ServeHTTP(w, r)
	if strings.HasSuffix(sub, "git-receive-pack") && r.Method == http.MethodPost {
		s.onPush(slug)
	}
}

func mustLookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return "/usr/bin/" + name
	}
	return p
}

// ---- merging ------------------------------------------------------------

var errConflict = errors.New("merge conflict")

var errNoBranch = errors.New("branch does not exist")

// mergePlan is a computed (not yet applied) merge of branch into base.
type mergePlan struct {
	key, base       string
	baseSHA, newSHA string
	noop            bool // branch already contained in base
}

// planMerge computes the merge commit without touching any ref, so several
// repos can be checked before any of them is changed.
func (s *Server) planMerge(slug, base, branch, message string) (*mergePlan, error) {
	baseSHA, err := s.gitStr(slug, "rev-parse", "--verify", "refs/heads/"+base)
	if err != nil {
		return nil, fmt.Errorf("base branch %s missing: %v", base, err)
	}
	headSHA, err := s.gitStr(slug, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return nil, errNoBranch
	}
	return s.planMergeFrom(slug, base, baseSHA, headSHA, message)
}

// planMergeSHA plans merging an arbitrary commit into base.
func (s *Server) planMergeSHA(slug, base, headSHA, message string) (*mergePlan, error) {
	baseSHA, err := s.gitStr(slug, "rev-parse", "--verify", "refs/heads/"+base)
	if err != nil {
		return nil, fmt.Errorf("base branch %s missing: %v", base, err)
	}
	return s.planMergeFrom(slug, base, baseSHA, headSHA, message)
}

func (s *Server) planMergeFrom(slug, base, baseSHA, headSHA, message string) (*mergePlan, error) {
	pl := &mergePlan{key: slug, base: base, baseSHA: baseSHA}
	if _, err := s.gitStr(slug, "merge-base", "--is-ancestor", headSHA, baseSHA); err == nil {
		pl.noop, pl.newSHA = true, baseSHA // already merged / no commits
		return pl, nil
	}
	if _, err := s.gitStr(slug, "merge-base", "--is-ancestor", baseSHA, headSHA); err == nil {
		pl.newSHA = headSHA // fast-forward
		return pl, nil
	}
	out, err := s.git(slug, nil, "merge-tree", "--write-tree", "--no-messages", baseSHA, headSHA)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errConflict, strings.TrimSpace(string(out)))
	}
	tree := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if pl.newSHA, err = s.gitStr(slug, "commit-tree", tree, "-p", baseSHA, "-p", headSHA, "-m", message); err != nil {
		return nil, err
	}
	return pl, nil
}

func (s *Server) applyMerge(pl *mergePlan) error {
	if pl.noop {
		return nil
	}
	if _, err := s.gitStr(pl.key, "update-ref", "refs/heads/"+pl.base, pl.newSHA, pl.baseSHA); err != nil {
		return err
	}
	s.onPush(pl.key)
	return nil
}

// mergeBranch merges branch into base inside the bare repo without a worktree.
func (s *Server) mergeBranch(slug, base, branch, message string) (string, error) {
	pl, err := s.planMerge(slug, base, branch, message)
	if errors.Is(err, errNoBranch) {
		return "", fmt.Errorf("branch %s does not exist on the server (was it pushed?)", branch)
	}
	if err != nil {
		return "", err
	}
	return pl.newSHA, s.applyMerge(pl)
}

// ---- browsing -----------------------------------------------------------

type TreeEntry struct {
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
	Name string `json:"name"`
}

func (s *Server) lsTree(slug, ref, path string) ([]TreeEntry, error) {
	spec := ref
	if path != "" {
		spec = ref + ":" + strings.Trim(path, "/")
	}
	out, err := s.git(slug, nil, "ls-tree", "-l", "-z", spec)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, line := range strings.Split(string(out), "\x00") {
		if line == "" {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(line[:tab])
		if len(f) < 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], SHA: f[2], Size: size, Name: line[tab+1:]})
	}
	return entries, nil
}

type CommitInfo struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    int64  `json:"date"`
	Subject string `json:"subject"`
	Refs    string `json:"refs"`
}

func (s *Server) gitLog(slug, ref string, n int) ([]CommitInfo, error) {
	args := []string{"log", "--format=%H%x1f%an%x1f%at%x1f%s%x1f%D%x1e", "-n", strconv.Itoa(n)}
	if ref == "" {
		args = append(args, "--all")
	} else {
		args = append(args, ref)
	}
	out, err := s.git(slug, nil, args...)
	if err != nil {
		return nil, err
	}
	var res []CommitInfo
	for _, rec := range strings.Split(string(out), "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x1f")
		if len(f) < 5 {
			continue
		}
		t, _ := strconv.ParseInt(f[2], 10, 64)
		res = append(res, CommitInfo{SHA: f[0], Author: f[1], Date: t * 1000, Subject: f[3], Refs: f[4]})
	}
	return res, nil
}

type BranchInfo struct {
	Name    string `json:"name"`
	SHA     string `json:"sha"`
	Date    int64  `json:"date"`
	Subject string `json:"subject"`
	Ahead   int    `json:"ahead"`
	Behind  int    `json:"behind"`
}

func (s *Server) branches(slug, base string) ([]BranchInfo, error) {
	out, err := s.git(slug, nil, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)%1f%(objectname)%1f%(committerdate:unix)%1f%(subject)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var res []BranchInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) < 4 {
			continue
		}
		t, _ := strconv.ParseInt(f[2], 10, 64)
		b := BranchInfo{Name: f[0], SHA: f[1], Date: t * 1000, Subject: f[3]}
		if b.Name != base {
			if c, err := s.gitStr(slug, "rev-list", "--left-right", "--count", base+"..."+b.Name); err == nil {
				fmt.Sscanf(c, "%d %d", &b.Behind, &b.Ahead)
			}
		}
		res = append(res, b)
	}
	return res, nil
}
