package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// tokenEntry is one record in tokens.json. `token` and `revoked` are consumed
// by the engram server (patch v2); the rest is admin metadata.
type tokenEntry struct {
	Token     string `json:"token"`
	Revoked   bool   `json:"revoked"`
	CreatedAt string `json:"created_at,omitempty"`
	Note      string `json:"note,omitempty"`
}

// tokenPublic is what the API exposes: never the full token.
type tokenPublic struct {
	Name      string `json:"name"`
	Revoked   bool   `json:"revoked"`
	CreatedAt string `json:"created_at,omitempty"`
	Note      string `json:"note,omitempty"`
	Suffix    string `json:"suffix"` // last 4 chars, for identification
}

var tokenNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// tokenStore manages tokens.json with atomic writes. It is the only writer;
// the engram server reloads the file when it changes.
type tokenStore struct {
	mu   sync.Mutex
	path string
	data map[string]tokenEntry
}

func newTokenStore(path string) (*tokenStore, error) {
	t := &tokenStore{path: path, data: map[string]tokenEntry{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// start empty; created on first write
		return t, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &t.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return t, nil
}

// writeLocked persists data atomically: tmp file in same dir, fsync, rename.
// Callers must hold t.mu.
func (t *tokenStore) writeLocked() error {
	b, err := json.MarshalIndent(t.data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(t.path), ".tokens-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// keep group-read permission for the engram server (0640)
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return err
	}
	return os.Rename(tmpName, t.path)
}

func (t *tokenStore) list() []tokenPublic {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]tokenPublic, 0, len(t.data))
	for name, e := range t.data {
		suffix := e.Token
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		out = append(out, tokenPublic{
			Name:      name,
			Revoked:   e.Revoked,
			CreatedAt: e.CreatedAt,
			Note:      e.Note,
			Suffix:    suffix,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// create generates a new token and returns it in full exactly once.
func (t *tokenStore) create(name, note string) (fullToken string, err error) {
	if !tokenNameRe.MatchString(name) {
		return "", fmt.Errorf("名字只能用小写字母、数字、`.` `_` `-`，最长 32 字符")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.data[name]; exists {
		return "", fmt.Errorf("名字 %q 已存在", name)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	fullToken = "eng_" + base64.RawURLEncoding.EncodeToString(raw)
	t.data[name] = tokenEntry{
		Token:     fullToken,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Note:      note,
	}
	if err := t.writeLocked(); err != nil {
		delete(t.data, name)
		return "", err
	}
	return fullToken, nil
}

func (t *tokenStore) setRevoked(name string, revoked bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.data[name]
	if !ok {
		return fmt.Errorf("没有名为 %q 的 token", name)
	}
	e.Revoked = revoked
	t.data[name] = e
	return t.writeLocked()
}

func (t *tokenStore) patch(name, note string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.data[name]
	if !ok {
		return fmt.Errorf("没有名为 %q 的 token", name)
	}
	e.Note = note
	t.data[name] = e
	return t.writeLocked()
}

func (t *tokenStore) delete(name string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.data[name]; !ok {
		return fmt.Errorf("没有名为 %q 的 token", name)
	}
	delete(t.data, name)
	return t.writeLocked()
}

// ---------- HTTP handlers ----------

func (s *server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tokens": s.tokens.list()})
}

func (s *server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	name := strings.ToLower(strings.TrimSpace(body.Name))
	tok, err := s.tokens.create(name, strings.TrimSpace(body.Note))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("token issued name=%s by %s", name, clientIP(r))
	// full token returned exactly once, here; never stored or listed again
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "token": tok})
}

func (s *server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.tokens.setRevoked(name, true); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("token revoked name=%s by %s", name, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleTokenUnrevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.tokens.setRevoked(name, false); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("token unrevoked name=%s by %s", name, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleTokenPatch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.tokens.patch(name, strings.TrimSpace(body.Note)); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.tokens.delete(name); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	log.Printf("token deleted name=%s by %s", name, clientIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
