// engram-admin is the read-only admin panel for the engram LAN MCP server.
// It manages the shared token list (issue/revoke per-person tokens) and
// serves a read-only view + global statistics over the engram SQLite store.
//
// The panel never writes to the engram database: it opens SQLite with
// mode=ro and, in production, filesystem permissions deny write anyway.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
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

	srv := &server{
		cfg:     cfg,
		db:      db,
		tokens:  tokens,
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
	// read-only memory APIs
	mux.HandleFunc("GET /api/stats/overview", srv.withAuth(srv.handleStatsOverview))
	mux.HandleFunc("GET /api/stats/timeseries", srv.withAuth(srv.handleStatsTimeseries))
	mux.HandleFunc("GET /api/stats/breakdown", srv.withAuth(srv.handleStatsBreakdown))
	mux.HandleFunc("GET /api/stats/topics", srv.withAuth(srv.handleStatsTopics))
	mux.HandleFunc("GET /api/memories", srv.withAuth(srv.handleMemories))
	mux.HandleFunc("GET /api/memories/{id}", srv.withAuth(srv.handleMemoryDetail))
	// token management APIs
	mux.HandleFunc("GET /api/tokens", srv.withAuth(srv.handleTokenList))
	mux.HandleFunc("POST /api/tokens", srv.withAuth(srv.handleTokenCreate))
	mux.HandleFunc("POST /api/tokens/{name}/revoke", srv.withAuth(srv.handleTokenRevoke))
	mux.HandleFunc("POST /api/tokens/{name}/unrevoke", srv.withAuth(srv.handleTokenUnrevoke))
	mux.HandleFunc("PATCH /api/tokens/{name}", srv.withAuth(srv.handleTokenPatch))
	mux.HandleFunc("DELETE /api/tokens/{name}", srv.withAuth(srv.handleTokenDelete))
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

type sessionStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]time.Time // token -> expiry
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, data: map[string]time.Time{}}
}

func (s *sessionStore) create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.data[tok] = time.Now().Add(s.ttl)
	// opportunistic GC
	for k, exp := range s.data {
		if time.Now().After(exp) {
			delete(s.data, k)
		}
	}
	s.mu.Unlock()
	return tok
}

func (s *sessionStore) valid(tok string) bool {
	s.mu.Lock()
	exp, ok := s.data[tok]
	s.mu.Unlock()
	return ok && time.Now().Before(exp)
}

func (s *sessionStore) drop(tok string) {
	s.mu.Lock()
	delete(s.data, tok)
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
	session *sessionStore
	limiter *loginLimiter
}

func (s *server) checkPassword(pw string) bool {
	if s.cfg.PassBcrypt != "" {
		return bcrypt.CompareHashAndPassword([]byte(s.cfg.PassBcrypt), []byte(pw)) == nil
	}
	// dev fallback, constant-time
	return subtle.ConstantTimeCompare([]byte(pw), []byte(s.cfg.DevPassword)) == 1
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		log.Printf("login rate-limited ip=%s", ip)
		writeErr(w, http.StatusTooManyRequests, "尝试太频繁，请 5 分钟后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if !s.checkPassword(body.Password) {
		s.limiter.record(ip)
		log.Printf("login failed ip=%s", ip)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	tok := s.session.create()
	http.SetCookie(w, &http.Cookie{
		Name:     "engram_admin_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	log.Printf("login ok ip=%s", ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	writeJSON(w, http.StatusOK, map[string]any{"user": "admin"})
}

func (s *server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("engram_admin_session")
		if err != nil || !s.session.valid(c.Value) {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	}
}

func clientIP(r *http.Request) string {
	// no trusted proxy in front; RemoteAddr is authoritative
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}
