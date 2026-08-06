// engram-admin is the read-only admin panel for the engram LAN MCP server.
// It manages the shared token list (issue/revoke per-person tokens) and
// serves a read-only view + global statistics over the engram SQLite store.
//
// The panel never writes to the engram database: it opens SQLite with
// mode=ro and, in production, filesystem permissions deny write anyway.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type config struct {
	Addr        string
	DBPath      string
	TokensFile  string
	AdminDB     string
	PassBcrypt  string
	SessionTTL  time.Duration
	DevPassword string // plaintext fallback, dev only
	// Display config, shown in the UI; safe to expose unauthenticated.
	Subtitle string // login page subtitle, e.g. "team memory hub"
	MCPURL   string // client onboarding hint shown on the tokens page
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() config {
	ttl, err := time.ParseDuration(envOr("ENGRAM_ADMIN_SESSION_TTL", "12h"))
	if err != nil {
		ttl = 12 * time.Hour
	}
	return config{
		Addr:        envOr("ENGRAM_ADMIN_ADDR", ":7441"),
		DBPath:      envOr("ENGRAM_DB", "/var/lib/engram-mcp/.engram/engram.db"),
		TokensFile:  envOr("ENGRAM_TOKENS_FILE", "/etc/engram-mcp/tokens.json"),
		AdminDB:     envOr("ENGRAM_ADMIN_DB", "/etc/engram-mcp/admin.db"),
		PassBcrypt:  strings.TrimSpace(os.Getenv("ENGRAM_ADMIN_PASS_BCRYPT")),
		SessionTTL:  ttl,
		DevPassword: strings.TrimSpace(os.Getenv("ENGRAM_ADMIN_PASSWORD")),
		Subtitle:    envOr("ENGRAM_ADMIN_SUBTITLE", "LAN shared memory hub"),
		MCPURL:      envOr("ENGRAM_ADMIN_MCP_URL", "http://127.0.0.1:7440/mcp"),
	}
}

func main() {
	log.SetPrefix("[engram-admin] ")
	if len(os.Args) > 1 && os.Args[1] == "hashpw" {
		hashpw()
		return
	}

	cfg := loadConfig()
	if cfg.PassBcrypt == "" && cfg.DevPassword == "" {
		log.Fatal("no admin credential: set ENGRAM_ADMIN_PASS_BCRYPT (or ENGRAM_ADMIN_PASSWORD for dev)")
	}

	db, err := openStoreRO(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer db.Close()

	tokens, err := newTokenStore(cfg.TokensFile)
	if err != nil {
		log.Fatalf("open token store: %v", err)
	}

	users, err := openUserStore(cfg.AdminDB)
	if err != nil {
		log.Fatalf("open user store: %v", err)
	}
	defer users.Close()
	// seed the initial admin from env (bcrypt preferred, dev password hashed on the fly)
	seedHash := cfg.PassBcrypt
	if seedHash == "" {
		if h, err := bcrypt.GenerateFromPassword([]byte(cfg.DevPassword), bcrypt.DefaultCost); err == nil {
			seedHash = string(h)
		}
	}
	if err := users.seedAdmin(seedHash); err != nil {
		log.Fatalf("seed admin: %v", err)
	}

	srv := &server{
		cfg:     cfg,
		db:      db,
		tokens:  tokens,
		users:   users,
		session: newSessionStore(cfg.SessionTTL),
		limiter: newLoginLimiter(5, 5*time.Minute),
	}

	mux := http.NewServeMux()
	// static UI (no auth needed for assets; data APIs are protected)
	mux.Handle("GET /", http.FileServer(http.FS(uiFS())))
	// auth
	mux.HandleFunc("POST /api/login", srv.handleLogin)
	mux.HandleFunc("POST /api/logout", srv.handleLogout)
	mux.HandleFunc("GET /api/whoami", srv.withAuth(srv.handleWhoami))
	// read-only memory APIs (any logged-in role)
	mux.HandleFunc("GET /api/stats/overview", srv.withAuth(srv.handleStatsOverview))
	mux.HandleFunc("GET /api/stats/timeseries", srv.withAuth(srv.handleStatsTimeseries))
	mux.HandleFunc("GET /api/stats/breakdown", srv.withAuth(srv.handleStatsBreakdown))
	mux.HandleFunc("GET /api/stats/topics", srv.withAuth(srv.handleStatsTopics))
	mux.HandleFunc("GET /api/memories", srv.withAuth(srv.handleMemories))
	mux.HandleFunc("GET /api/memories/{id}", srv.withAuth(srv.handleMemoryDetail))
	// self-service (any logged-in role)
	mux.HandleFunc("GET /api/me", srv.withAuth(srv.handleMe))
	mux.HandleFunc("POST /api/me/password", srv.withAuth(srv.handleMePassword))
	mux.HandleFunc("POST /api/me/token", srv.withAuth(srv.handleMeToken))
	// token management APIs (admin only)
	mux.HandleFunc("GET /api/tokens", srv.withAdmin(srv.handleTokenList))
	mux.HandleFunc("POST /api/tokens", srv.withAdmin(srv.handleTokenCreate))
	mux.HandleFunc("POST /api/tokens/{name}/revoke", srv.withAdmin(srv.handleTokenRevoke))
	mux.HandleFunc("POST /api/tokens/{name}/unrevoke", srv.withAdmin(srv.handleTokenUnrevoke))
	mux.HandleFunc("PATCH /api/tokens/{name}", srv.withAdmin(srv.handleTokenPatch))
	mux.HandleFunc("DELETE /api/tokens/{name}", srv.withAdmin(srv.handleTokenDelete))
	// user management APIs (admin only)
	mux.HandleFunc("GET /api/users", srv.withAdmin(srv.handleUserList))
	mux.HandleFunc("POST /api/users", srv.withAdmin(srv.handleUserCreate))
	mux.HandleFunc("POST /api/users/{name}/disable", srv.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		srv.handleUserSetDisabled(w, r, true)
	}))
	mux.HandleFunc("POST /api/users/{name}/enable", srv.withAdmin(func(w http.ResponseWriter, r *http.Request) {
		srv.handleUserSetDisabled(w, r, false)
	}))
	mux.HandleFunc("POST /api/users/{name}/reset-password", srv.withAdmin(srv.handleUserResetPassword))
	mux.HandleFunc("DELETE /api/users/{name}", srv.withAdmin(srv.handleUserDelete))
	// health and display meta, unauthenticated
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "engram-admin"})
	})
	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"subtitle": cfg.Subtitle, "mcp_url": cfg.MCPURL,
		})
	})

	log.Printf("listening on %s (db=%s ro, tokens=%s)", cfg.Addr, cfg.DBPath, cfg.TokensFile)
	log.Fatal(http.ListenAndServe(cfg.Addr, securityHeaders(mux)))
}

// hashpw prints a bcrypt hash of a password read from stdin (first line).
func hashpw() {
	fmt.Fprint(os.Stderr, "password on stdin: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(pw) == 0 {
		log.Fatalf("read password: %v", err)
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 8 {
		log.Fatal("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(h))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---------- sessions ----------

type sessionEntry struct {
	username string
	role     string
	expiry   time.Time
}

type sessionStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]sessionEntry // token -> entry
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, data: map[string]sessionEntry{}}
}

func (s *sessionStore) create(username, role string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.data[tok] = sessionEntry{username: username, role: role, expiry: time.Now().Add(s.ttl)}
	// opportunistic GC
	for k, e := range s.data {
		if time.Now().After(e.expiry) {
			delete(s.data, k)
		}
	}
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(tok string) (sessionEntry, bool) {
	s.mu.Lock()
	e, ok := s.data[tok]
	s.mu.Unlock()
	return e, ok && time.Now().Before(e.expiry)
}

func (s *sessionStore) drop(tok string) {
	s.mu.Lock()
	delete(s.data, tok)
	s.mu.Unlock()
}

func (s *sessionStore) dropByUser(username string) {
	s.mu.Lock()
	for k, e := range s.data {
		if e.username == username {
			delete(s.data, k)
		}
	}
	s.mu.Unlock()
}

// ---------- login rate limit ----------

type loginLimiter struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	attempts map[string][]time.Time
}

func newLoginLimiter(max int, window time.Duration) *loginLimiter {
	return &loginLimiter{max: max, window: window, attempts: map[string][]time.Time{}}
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	keep := l.attempts[ip][:0]
	for _, t := range l.attempts[ip] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	l.attempts[ip] = keep
	return len(keep) < l.max
}

func (l *loginLimiter) record(ip string) {
	l.mu.Lock()
	l.attempts[ip] = append(l.attempts[ip], time.Now())
	l.mu.Unlock()
}

// ---------- handlers: auth ----------

type server struct {
	cfg     config
	db      *store
	tokens  *tokenStore
	users   *userStore
	session *sessionStore
	limiter *loginLimiter
}

// ctxKey is the context key for the authenticated session entry.
type ctxKey struct{}

var sessionKey ctxKey

func sessionOf(r *http.Request) sessionEntry {
	return r.Context().Value(sessionKey).(sessionEntry)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		log.Printf("login rate-limited ip=%s", ip)
		writeErr(w, http.StatusTooManyRequests, "尝试太频繁，请 5 分钟后再试")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	username := strings.ToLower(strings.TrimSpace(body.Username))
	u, ok := s.users.checkPassword(username, body.Password)
	if !ok {
		s.limiter.record(ip)
		log.Printf("login failed user=%q ip=%s", username, ip)
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok := s.session.create(u.Username, u.Role)
	s.users.touchLogin(u.Username)
	http.SetCookie(w, &http.Cookie{
		Name:     "engram_admin_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	log.Printf("login ok user=%s role=%s ip=%s", u.Username, u.Role, ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": u.Username, "role": u.Role})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("engram_admin_session"); err == nil {
		s.session.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "engram_admin_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	e := sessionOf(r)
	writeJSON(w, http.StatusOK, map[string]any{"username": e.username, "role": e.role})
}

func (s *server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("engram_admin_session")
		var e sessionEntry
		var ok bool
		if err == nil {
			e, ok = s.session.valid(c.Value)
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, e)))
	}
}

// withAdmin restricts a route to admin-role sessions.
func (s *server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		if sessionOf(r).role != "admin" {
			writeErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		next(w, r)
	})
}

func clientIP(r *http.Request) string {
	// no trusted proxy in front; RemoteAddr is authoritative
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}
