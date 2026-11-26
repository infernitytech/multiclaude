package server

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Authentication model
//   - Humans: username + password. The web UI uses a session cookie; git
//     clients use HTTP Basic auth with the same credentials.
//   - Machines: a runner enrolls once with the server-wide *join token* and
//     receives its own *node token* (Bearer). Removing a machine in the UI
//     revokes its token. Git access from runners uses the node token too.

const (
	sessionCookie = "mc_session"
	sessionTTL    = 30 * 24 * time.Hour
	pbkdfIter     = 210_000
)

const authSchema = `
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE COLLATE NOCASE,
  pw_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions(
  token_hash TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	CreatedAt int64  `json:"created_at"`
}

// ---- primitives ---------------------------------------------------------------

func randToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	k, err := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

func checkPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2-sha256" {
		return false
	}
	iter, _ := strconv.Atoi(p[1])
	salt, err1 := base64.RawStdEncoding.DecodeString(p[2])
	want, err2 := base64.RawStdEncoding.DecodeString(p[3])
	if iter <= 0 || err1 != nil || err2 != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(k, want) == 1
}

func (s *Server) setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Server) setSetting(key, value string) {
	s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}

// joinToken returns the server-wide enrollment token, creating it on first use.
func (s *Server) joinToken() string {
	if t := s.setting("join_token"); t != "" {
		return t
	}
	t := randToken("mcj_")
	s.setSetting("join_token", t)
	return t
}

// ---- users ------------------------------------------------------------------------

func validUsername(u string) bool {
	if len(u) < 1 || len(u) > 64 {
		return false
	}
	for _, c := range u {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-@", c)) {
			return false
		}
	}
	return true
}

func (s *Server) SetPassword(username, pw string) error {
	if !validUsername(username) {
		return errors.New("invalid username (letters, digits and . _ - @ only)")
	}
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET pw_hash=? WHERE username=?`, h, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = s.db.Exec(`INSERT INTO users(username,pw_hash,created_at) VALUES(?,?,?)`, username, h, now())
		return err
	}
	// a new password logs out every session of that user
	s.db.Exec(`DELETE FROM sessions WHERE user_id=(SELECT id FROM users WHERE username=?)`, username)
	s.forgetBasicCache()
	return nil
}

// ResetPassword sets a fresh random password (creating the user if needed) and returns it.
func (s *Server) ResetPassword(username string) (string, error) {
	pw := randToken("")[:20]
	return pw, s.SetPassword(username, pw)
}

// ensureAdmin creates the first account so a fresh server is never open.
func (s *Server) ensureAdmin() {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n > 0 {
		return
	}
	user := firstNonEmpty(os.Getenv("MC_ADMIN_USER"), "admin")
	if pw := os.Getenv("MC_ADMIN_PASSWORD"); pw != "" {
		if err := s.SetPassword(user, pw); err != nil {
			log.Fatalf("MC_ADMIN_PASSWORD: %v", err)
		}
		log.Printf("created user %q with the password from MC_ADMIN_PASSWORD", user)
		return
	}
	pw, err := s.ResetPassword(user)
	if err != nil {
		log.Fatal(err)
	}
	f := filepath.Join(s.DataDir, "initial-admin-password.txt")
	os.WriteFile(f, []byte(user+"\n"+pw+"\n"), 0o600)
	log.Printf("\n\n  ==> First start: created user %q with password  %s\n      (also saved to %s — change it on the Users & account page; the file is deleted then)\n", user, pw, f)
}

func (s *Server) userByName(name string) (*User, string, error) {
	var u User
	var h string
	err := s.db.QueryRow(`SELECT id,username,created_at,pw_hash FROM users WHERE username=?`, name).Scan(&u.ID, &u.Username, &u.CreatedAt, &h)
	return &u, h, err
}

// ---- brute-force protection -------------------------------------------------------

type failInfo struct {
	n     int
	until time.Time
}

var (
	failMu sync.Mutex
	fails  = map[string]*failInfo{}
)

func loginBlocked(key string) time.Duration {
	failMu.Lock()
	defer failMu.Unlock()
	if f := fails[key]; f != nil && time.Now().Before(f.until) {
		return time.Until(f.until)
	}
	return 0
}

func loginFailed(key string) {
	failMu.Lock()
	defer failMu.Unlock()
	f := fails[key]
	if f == nil {
		f = &failInfo{}
		fails[key] = f
	}
	f.n++
	if f.n >= 5 {
		d := time.Minute << min(f.n-5, 4) // 1, 2, 4, 8, 16 minutes
		f.until = time.Now().Add(d)
	}
	if len(fails) > 10000 { // don't grow without bound under attack
		for k, v := range fails {
			if time.Now().After(v.until) {
				delete(fails, k)
			}
		}
	}
}

func loginOK(key string) {
	failMu.Lock()
	delete(fails, key)
	failMu.Unlock()
}

// clientIP trusts X-Forwarded-For only from a reverse proxy on a private network.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return host
}

// verifyLogin checks credentials with rate limiting per client IP.
func (s *Server) verifyLogin(r *http.Request, user, pw string) (*User, error) {
	key := clientIP(r)
	if d := loginBlocked(key); d > 0 {
		return nil, fmt.Errorf("too many failed logins — try again in %s", d.Round(time.Second))
	}
	u, h, err := s.userByName(user)
	if err != nil || !checkPassword(h, pw) {
		if err != nil {
			checkPassword("pbkdf2-sha256$210000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", pw) // same timing
		}
		loginFailed(key)
		log.Printf("auth: failed login for %q from %s", user, key)
		return nil, errors.New("wrong username or password")
	}
	loginOK(key)
	return u, nil
}

// Git clients send Basic auth on every request; cache good credentials briefly
// so PBKDF2 doesn't run hundreds of times per clone.
var (
	basicMu    sync.Mutex
	basicCache = map[string]time.Time{}
)

func (s *Server) forgetBasicCache() {
	basicMu.Lock()
	basicCache = map[string]time.Time{}
	basicMu.Unlock()
}

func (s *Server) basicUserOK(r *http.Request, user, pw string) bool {
	k := tokenHash(user + "\x00" + pw)
	basicMu.Lock()
	exp, ok := basicCache[k]
	basicMu.Unlock()
	if ok && time.Now().Before(exp) {
		return true
	}
	if _, err := s.verifyLogin(r, user, pw); err != nil {
		return false
	}
	basicMu.Lock()
	basicCache[k] = time.Now().Add(10 * time.Minute)
	basicMu.Unlock()
	return true
}

// ---- request identity -------------------------------------------------------------

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxNode
)

func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUser).(*User)
	return u
}

// nodeFrom returns the id of the machine that authenticated this request.
func nodeFrom(r *http.Request) string {
	n, _ := r.Context().Value(ctxNode).(string)
	return n
}

func (s *Server) sessionUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var u User
	err = s.db.QueryRow(`SELECT u.id,u.username,u.created_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, tokenHash(c.Value), now()).Scan(&u.ID, &u.Username, &u.CreatedAt)
	if err != nil {
		return nil
	}
	return &u
}

func (s *Server) nodeByToken(tok string) string {
	if !strings.HasPrefix(tok, "mcn_") {
		return ""
	}
	var id string
	s.db.QueryRow(`SELECT id FROM nodes WHERE token_hash=?`, tokenHash(tok)).Scan(&id)
	return id
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// sameOrigin rejects cross-site state-changing requests (CSRF defence in
// addition to SameSite cookies).
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	host := firstNonEmpty(r.Header.Get("X-Forwarded-Host"), r.Host)
	return strings.EqualFold(u.Host, host)
}

func unauthorized(w http.ResponseWriter, msg string) { httpErr(w, 401, msg) }

// authMiddleware enforces authentication for every route.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		p := r.URL.Path
		ctx := r.Context()
		switch {
		// public: the SPA shell, login, the installer and runner binaries
		case p == "/api/auth", p == "/api/login", p == "/api/logout",
			p == "/install.sh", strings.HasPrefix(p, "/download/"):

		// machines: node token
		case p == "/api/runner/register":
			// the handler accepts a node token or the join token
			if id := s.nodeByToken(bearer(r)); id != "" {
				ctx = context.WithValue(ctx, ctxNode, id)
			}
		case strings.HasPrefix(p, "/api/runner/"), strings.HasPrefix(p, "/api/agent/"):
			id := s.nodeByToken(bearer(r))
			if id == "" {
				unauthorized(w, "invalid or revoked machine token — re-enroll this machine (see Machines → Add a machine)")
				return
			}
			ctx = context.WithValue(ctx, ctxNode, id)

		// git: Basic auth (user+password, or anything+node token) or Bearer node token
		case strings.HasPrefix(p, "/git/"):
			if b := bearer(r); b != "" && subtle.ConstantTimeCompare([]byte(b), []byte(s.internalToken)) == 1 {
				break
			}
			if id := s.nodeByToken(bearer(r)); id != "" {
				ctx = context.WithValue(ctx, ctxNode, id)
				break
			}
			if user, pw, ok := r.BasicAuth(); ok {
				if id := s.nodeByToken(pw); id != "" {
					ctx = context.WithValue(ctx, ctxNode, id)
					break
				}
				if s.basicUserOK(r, user, pw) {
					break
				}
			}
			h.Set("WWW-Authenticate", `Basic realm="MultiClaude", charset="UTF-8"`)
			http.Error(w, "authentication required", 401)
			return

		// artifacts: machines (agents) use their node token
		case isArtifactPath(p) && bearer(r) != "" && s.nodeByToken(bearer(r)) != "":
			ctx = context.WithValue(ctx, ctxNode, s.nodeByToken(bearer(r)))

		// UI API: session cookie
		case strings.HasPrefix(p, "/api/"):
			u := s.sessionUser(r)
			if u == nil { // scripts: curl -u user:password
				if name, pw, ok := r.BasicAuth(); ok && s.basicUserOK(r, name, pw) {
					u, _, _ = s.userByName(name)
				}
			}
			if u == nil {
				unauthorized(w, "not logged in")
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" && !sameOrigin(r) {
				httpErr(w, 403, "cross-origin request refused")
				return
			}
			ctx = context.WithValue(ctx, ctxUser, u)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---- handlers ---------------------------------------------------------------------

func (s *Server) apiAuth(w http.ResponseWriter, r *http.Request) {
	if u := s.sessionUser(r); u != nil {
		writeJSON(w, map[string]any{"user": u})
		return
	}
	writeJSON(w, map[string]any{"user": nil})
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		httpErr(w, 405, "POST only")
		return
	}
	if !sameOrigin(r) {
		httpErr(w, 403, "cross-origin request refused")
		return
	}
	var in struct{ Username, Password string }
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	u, err := s.verifyLogin(r, strings.TrimSpace(in.Username), in.Password)
	if err != nil {
		time.Sleep(500 * time.Millisecond)
		unauthorized(w, err.Error())
		return
	}
	tok := randToken("mcs_")
	t := now()
	s.db.Exec(`DELETE FROM sessions WHERE expires_at<?`, t)
	s.db.Exec(`INSERT INTO sessions(token_hash,user_id,created_at,expires_at) VALUES(?,?,?,?)`, tokenHash(tok), u.ID, t, t+sessionTTL.Milliseconds())
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: isHTTPS(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	log.Printf("auth: %s logged in from %s", u.Username, clientIP(r))
	writeJSON(w, map[string]any{"user": u})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: isHTTPS(r), MaxAge: -1})
	w.WriteHeader(204)
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id,username,created_at FROM users ORDER BY username`)
	if err != nil {
		httpErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Username, &u.CreatedAt)
		out = append(out, u)
	}
	writeJSON(w, out)
}

func (s *Server) apiCreateUser(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if _, _, err := s.userByName(in.Username); err == nil {
		httpErr(w, 409, "user already exists")
		return
	}
	if err := s.SetPassword(in.Username, in.Password); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	log.Printf("auth: %s created user %s", userFrom(r).Username, in.Username)
	w.WriteHeader(204)
}

func (s *Server) apiUpdateUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
		Current  string `json:"current"`
	}
	if err := readJSON(r, &in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var name, h string
	if err := s.db.QueryRow(`SELECT username,pw_hash FROM users WHERE id=?`, id).Scan(&name, &h); err != nil {
		httpErr(w, 404, "no such user")
		return
	}
	me := userFrom(r)
	if id == me.ID && !checkPassword(h, in.Current) {
		httpErr(w, 403, "current password is wrong")
		return
	}
	if err := s.SetPassword(name, in.Password); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	if id == me.ID { // SetPassword logged everyone out; keep this browser logged in
		tok := randToken("mcs_")
		t := now()
		s.db.Exec(`INSERT INTO sessions(token_hash,user_id,created_at,expires_at) VALUES(?,?,?,?)`, tokenHash(tok), id, t, t+sessionTTL.Milliseconds())
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: isHTTPS(r),
			SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
		os.Remove(filepath.Join(s.DataDir, "initial-admin-password.txt"))
	}
	log.Printf("auth: %s changed the password of %s", me.Username, name)
	w.WriteHeader(204)
}

func (s *Server) apiDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == userFrom(r).ID {
		httpErr(w, 400, "you can't delete your own account")
		return
	}
	s.db.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	s.db.Exec(`DELETE FROM users WHERE id=?`, id)
	s.forgetBasicCache()
	w.WriteHeader(204)
}

// apiRunnerSetup gives the UI the join token and ready-to-paste commands.
func (s *Server) apiRunnerSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" { // rotate
		s.setSetting("join_token", randToken("mcj_"))
	}
	base := baseURL(r)
	tok := s.joinToken()
	writeJSON(w, map[string]any{
		"join_token": tok,
		"server":     base,
		"install":    fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --token %s", base, tok),
		"manual":     fmt.Sprintf("multiclaude-runner enroll --server %s --token %s && multiclaude-runner run", base, tok),
	})
}

// enroll validates a runner's credentials at registration. It returns the node
// id to use and, for a newly enrolled machine, a fresh node token.
func (s *Server) enroll(r *http.Request, reqNodeID, joinTok string) (id, newToken string, err error) {
	if id := nodeFrom(r); id != "" {
		return id, "", nil
	}
	if joinTok == "" || subtle.ConstantTimeCompare([]byte(joinTok), []byte(s.joinToken())) != 1 {
		if joinTok == "" && bearer(r) != "" {
			return "", "", errors.New("this machine's token was revoked (machine removed in the UI). Enroll it again with the join token from Machines → Add a machine")
		}
		return "", "", errors.New("invalid join token — copy the current one from Machines → Add a machine")
	}
	id = newID()
	// Keep the identity of a machine that registered before auth existed.
	if reqNodeID != "" {
		var th string
		if e := s.db.QueryRow(`SELECT token_hash FROM nodes WHERE id=?`, reqNodeID).Scan(&th); e == nil && th == "" {
			id = reqNodeID
		} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return "", "", e
		}
	}
	return id, randToken("mcn_"), nil
}

// isArtifactPath: /api/projects/<slug>/artifacts[/…]
func isArtifactPath(p string) bool {
	parts := strings.SplitN(strings.TrimPrefix(p, "/api/projects/"), "/", 3)
	return strings.HasPrefix(p, "/api/projects/") && len(parts) >= 2 && parts[1] == "artifacts"
}
