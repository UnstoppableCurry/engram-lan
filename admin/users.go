package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// userRecord is one panel account. Admin accounts manage everything; user
// accounts see memory/stats and self-service their own key + password.
type userRecord struct {
	Username   string  `json:"username"`
	Role       string  `json:"role"` // "admin" | "user"
	Disabled   bool    `json:"disabled"`
	CreatedAt  string  `json:"created_at"`
	LastLogin  *string `json:"last_login"`
	PassBcrypt string  `json:"-"`
}

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// userStore persists panel accounts in the panel's OWN sqlite file
// (never the engram memory base, which stays read-only).
type userStore struct {
	db *sql.DB
}

func openUserStore(path string) (*userStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(3000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer keeps WAL simple
	u := &userStore{db: db}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (
		username    TEXT PRIMARY KEY,
		role        TEXT NOT NULL DEFAULT 'user',
		pass_bcrypt TEXT NOT NULL,
		disabled    INTEGER NOT NULL DEFAULT 0,
		created_at  TEXT NOT NULL,
		last_login  TEXT
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return u, nil
}

func (u *userStore) Close() error { return u.db.Close() }

// seedAdmin creates the initial admin from the env-provided bcrypt hash when
// no admin exists yet. The env var stays the recovery path.
func (u *userStore) seedAdmin(hash string) error {
	if hash == "" {
		return nil
	}
	var n int
	if err := u.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := u.db.Exec(
		`INSERT INTO users (username, role, pass_bcrypt, disabled, created_at)
		 VALUES ('admin', 'admin', ?, 0, ?)`, hash, time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		log.Printf("seeded initial admin account 'admin' from ENGRAM_ADMIN_PASS_BCRYPT")
	}
	return err
}

var errNoSuchUser = errors.New("用户不存在")

func (u *userStore) get(username string) (*userRecord, error) {
	r := &userRecord{}
	var last sql.NullString
	err := u.db.QueryRow(
		`SELECT username, role, pass_bcrypt, disabled, created_at, last_login
		   FROM users WHERE username = ?`, username,
	).Scan(&r.Username, &r.Role, &r.PassBcrypt, &r.Disabled, &r.CreatedAt, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoSuchUser
	}
	if err != nil {
		return nil, err
	}
	if last.Valid {
		r.LastLogin = &last.String
	}
	return r, nil
}

func (u *userStore) list() ([]userRecord, error) {
	rows, err := u.db.Query(
		`SELECT username, role, pass_bcrypt, disabled, created_at, last_login
		   FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userRecord
	for rows.Next() {
		var r userRecord
		var last sql.NullString
		if err := rows.Scan(&r.Username, &r.Role, &r.PassBcrypt, &r.Disabled, &r.CreatedAt, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			r.LastLogin = &last.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (u *userStore) create(username, password, role string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("用户名只能用小写字母、数字、`.` `_` `-`，最长 32 字符")
	}
	if role != "user" && role != "admin" {
		role = "user"
	}
	if len(password) < 8 {
		return fmt.Errorf("初始密码至少 8 位")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = u.db.Exec(
		`INSERT INTO users (username, role, pass_bcrypt, disabled, created_at)
		 VALUES (?, ?, ?, 0, ?)`, username, role, string(h), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("用户名 %q 已存在", username)
		}
		return err
	}
	return nil
}

func (u *userStore) setPassword(username, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("密码至少 8 位")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := u.db.Exec(`UPDATE users SET pass_bcrypt = ? WHERE username = ?`, string(h), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoSuchUser
	}
	return nil
}

func (u *userStore) setDisabled(username string, disabled bool) error {
	res, err := u.db.Exec(`UPDATE users SET disabled = ? WHERE username = ?`, disabled, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoSuchUser
	}
	return nil
}

func (u *userStore) deleteUser(username string) error {
	res, err := u.db.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoSuchUser
	}
	return nil
}

func (u *userStore) touchLogin(username string) {
	_, _ = u.db.Exec(`UPDATE users SET last_login = ? WHERE username = ?`,
		time.Now().UTC().Format(time.RFC3339), username)
}

func (u *userStore) checkPassword(username, password string) (*userRecord, bool) {
	r, err := u.get(username)
	if err != nil || r.Disabled {
		return nil, false
	}
	ok := bcrypt.CompareHashAndPassword([]byte(r.PassBcrypt), []byte(password)) == nil
	return r, ok
}

// ---------- HTTP handlers: self-service ----------

func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	u, err := s.users.get(sess.username)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// owner sees their own full token — self-service key model
	tok, tokErr := s.tokens.get(u.Username)
	resp := map[string]any{
		"username": u.Username, "role": u.Role,
		"created_at": u.CreatedAt, "last_login": u.LastLogin,
		"has_token": tokErr == nil,
	}
	if tokErr == nil {
		resp["token"] = tok.Token
		resp["token_revoked"] = tok.Revoked
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleMePassword(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	var body struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if _, ok := s.users.checkPassword(sess.username, body.Old); !ok {
		writeErr(w, http.StatusForbidden, "原密码错误")
		return
	}
	if err := s.users.setPassword(sess.username, body.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("password changed user=%s ip=%s", sess.username, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMeToken issues or regenerates the caller's own key. The old key dies
// immediately (tokens.json hot-reloads).
func (s *server) handleMeToken(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	tok, err := s.tokens.regenerate(sess.username, "self-service")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("token regenerated by owner user=%s ip=%s", sess.username, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"token": tok})
}

// ---------- HTTP handlers: user management (admin) ----------

func (s *server) handleUserList(w http.ResponseWriter, r *http.Request) {
	users, err := s.users.list()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Username))
	if err := s.users.create(name, body.Password, body.Role); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("user created name=%s role=%s by=%s", name, body.Role, sessionOf(r).username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleUserSetDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	name := r.PathValue("name")
	if name == sessionOf(r).username {
		writeErr(w, http.StatusBadRequest, "不能禁用自己")
		return
	}
	if err := s.users.setDisabled(name, disabled); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if disabled {
		s.session.dropByUser(name)
	}
	log.Printf("user disabled=%v name=%s by=%s", disabled, name, sessionOf(r).username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.users.setPassword(name, body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.session.dropByUser(name)
	log.Printf("password reset name=%s by=%s", name, sessionOf(r).username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == sessionOf(r).username {
		writeErr(w, http.StatusBadRequest, "不能删除自己")
		return
	}
	if err := s.users.deleteUser(name); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.session.dropByUser(name)
	// the user's MCP token dies with the account
	if err := s.tokens.delete(name); err != nil {
		log.Printf("user deleted but token cleanup failed name=%s: %v", name, err)
	}
	log.Printf("user deleted name=%s by=%s", name, sessionOf(r).username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
