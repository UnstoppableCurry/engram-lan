package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// store is a strictly read-only handle to the engram SQLite database.
// The DSN uses mode=ro so any write attempt fails at the driver level;
// production additionally relies on filesystem permissions (0640, group ro).
type store struct {
	db *sql.DB
}

func openStoreRO(path string) (*store, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

// ---------- stats ----------

type overview struct {
	Observations  int     `json:"observations"`
	Deleted       int     `json:"deleted"`
	Pinned        int     `json:"pinned"`
	Sessions      int     `json:"sessions"`
	Projects      int     `json:"projects"`
	Prompts       int     `json:"prompts"`
	DuplicatesSum int     `json:"duplicates_sum"`
	LastWriteAt   *string `json:"last_write_at"`
	DBSizeMB      float64 `json:"db_size_mb"`
}

func (s *store) statsOverview() (*overview, error) {
	o := &overview{}
	q := `SELECT
	  (SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL),
	  (SELECT COUNT(*) FROM observations WHERE deleted_at IS NOT NULL),
	  (SELECT COUNT(*) FROM observations WHERE deleted_at IS NULL AND pinned),
	  (SELECT COUNT(*) FROM sessions),
	  (SELECT COUNT(DISTINCT project) FROM observations WHERE deleted_at IS NULL),
	  (SELECT COUNT(*) FROM user_prompts),
	  (SELECT COALESCE(SUM(duplicate_count),0) FROM observations WHERE deleted_at IS NULL)`
	if err := s.db.QueryRow(q).Scan(
		&o.Observations, &o.Deleted, &o.Pinned, &o.Sessions,
		&o.Projects, &o.Prompts, &o.DuplicatesSum,
	); err != nil {
		return nil, err
	}
	var last sql.NullString
	if err := s.db.QueryRow(
		`SELECT MAX(created_at) FROM observations WHERE deleted_at IS NULL`,
	).Scan(&last); err == nil && last.Valid {
		o.LastWriteAt = &last.String
	}
	var pageCount, pageSize int64
	_ = s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount)
	_ = s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
	o.DBSizeMB = float64(pageCount*pageSize) / (1024 * 1024)
	return o, nil
}

type dayPoint struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

func (s *store) statsTimeseries(days int) ([]dayPoint, error) {
	if days <= 0 || days > 366 {
		days = 90
	}
	rows, err := s.db.Query(
		`SELECT substr(created_at, 1, 10) AS day, COUNT(*)
		   FROM observations
		  WHERE deleted_at IS NULL
		    AND substr(created_at, 1, 10) >= date('now', ?)
		  GROUP BY day ORDER BY day`, fmt.Sprintf("-%d days", days-1))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]int{}
	for rows.Next() {
		var p dayPoint
		if err := rows.Scan(&p.Day, &p.Count); err != nil {
			return nil, err
		}
		byDay[p.Day] = p.Count
	}
	// fill gaps so the chart has a continuous axis
	out := make([]dayPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		var d string
		_ = s.db.QueryRow(`SELECT date('now', ?)`, fmt.Sprintf("-%d days", i)).Scan(&d)
		out = append(out, dayPoint{Day: d, Count: byDay[d]})
	}
	return out, nil
}

type bucket struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func (s *store) statsBreakdown() (byType, byProject, byScope []bucket, err error) {
	query := func(col string) ([]bucket, error) {
		// col is a fixed internal identifier, never user input
		rows, err := s.db.Query(
			`SELECT COALESCE(NULLIF(` + col + `, ''), '(空)') AS k, COUNT(*)
			   FROM observations WHERE deleted_at IS NULL
			  GROUP BY k ORDER BY COUNT(*) DESC, k`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []bucket
		for rows.Next() {
			var b bucket
			if err := rows.Scan(&b.Key, &b.Count); err != nil {
				return nil, err
			}
			out = append(out, b)
		}
		return out, rows.Err()
	}
	if byType, err = query("type"); err != nil {
		return
	}
	if byProject, err = query("project"); err != nil {
		return
	}
	if byScope, err = query("scope"); err != nil {
		return
	}
	return
}

type topicRow struct {
	TopicKey   string `json:"topic_key"`
	Title      string `json:"title"`
	Project    string `json:"project"`
	Revisions  int    `json:"revisions"`
	Duplicates int    `json:"duplicates"`
	UpdatedAt  string `json:"updated_at"`
}

func (s *store) statsTopics(limit int) ([]topicRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(
		`SELECT topic_key, COALESCE(title,''), COALESCE(project,''),
		        revision_count, duplicate_count, COALESCE(updated_at,'')
		   FROM observations
		  WHERE deleted_at IS NULL AND topic_key IS NOT NULL AND topic_key != ''
		  ORDER BY revision_count DESC, duplicate_count DESC, updated_at DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicRow
	for rows.Next() {
		var t topicRow
		if err := rows.Scan(&t.TopicKey, &t.Title, &t.Project,
			&t.Revisions, &t.Duplicates, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- memories browse/search ----------

type memory struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Content    string `json:"content,omitempty"`
	Project    string `json:"project"`
	Scope      string `json:"scope"`
	TopicKey   string `json:"topic_key"`
	SessionID  string `json:"session_id"`
	Revisions  int    `json:"revisions"`
	Duplicates int    `json:"duplicates"`
	Pinned     bool   `json:"pinned"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

const memCols = `o.id, COALESCE(o.type,''), COALESCE(o.title,''), COALESCE(o.project,''),
       COALESCE(o.scope,''), COALESCE(o.topic_key,''), COALESCE(o.session_id,''),
       o.revision_count, o.duplicate_count, o.pinned, COALESCE(o.created_at,''), COALESCE(o.updated_at,'')`

func scanMemory(row interface{ Scan(...any) error }, withContent bool) (*memory, error) {
	m := &memory{}
	if withContent {
		return m, row.Scan(&m.ID, &m.Type, &m.Title, &m.Project, &m.Scope,
			&m.TopicKey, &m.SessionID, &m.Revisions, &m.Duplicates, &m.Pinned,
			&m.CreatedAt, &m.UpdatedAt, &m.Content)
	}
	return m, row.Scan(&m.ID, &m.Type, &m.Title, &m.Project, &m.Scope,
		&m.TopicKey, &m.SessionID, &m.Revisions, &m.Duplicates, &m.Pinned,
		&m.CreatedAt, &m.UpdatedAt)
}

// sanitizeFTS quotes every whitespace-separated term for FTS5 MATCH,
// mirroring engram's own sanitizeFTS (double internal quotes).
func sanitizeFTS(q string) string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
	}
	return strings.Join(out, " ")
}

func (s *store) listMemories(q, project, typ string, page, size int) (items []memory, total int, err error) {
	if size <= 0 || size > 100 {
		size = 20
	}
	if page <= 0 {
		page = 1
	}

	var where []string
	var args []any
	where = append(where, "o.deleted_at IS NULL")
	if project != "" {
		where = append(where, "o.project = ?")
		args = append(args, project)
	}
	if typ != "" {
		where = append(where, "o.type = ?")
		args = append(args, typ)
	}

	from := `FROM observations o`
	if strings.TrimSpace(q) != "" {
		from += ` JOIN observations_fts fts ON fts.rowid = o.id`
		where = append(where, `observations_fts MATCH ?`)
		args = append(args, sanitizeFTS(q))
	}
	whereSQL := ` WHERE ` + strings.Join(where, " AND ")

	if err = s.db.QueryRow(`SELECT COUNT(*) `+from+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listArgs := append([]any{}, args...)
	listArgs = append(listArgs, size, (page-1)*size)
	rows, err := s.db.Query(
		`SELECT `+memCols+` `+from+whereSQL+
			` ORDER BY o.created_at DESC, o.id DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMemory(rows, false)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *m)
	}
	return items, total, rows.Err()
}

func (s *store) getMemory(id int64) (*memory, error) {
	row := s.db.QueryRow(
		`SELECT `+memCols+`, COALESCE(content,'')
		   FROM observations o WHERE o.id = ? AND o.deleted_at IS NULL`, id)
	m, err := scanMemory(row, true)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// distinctProjects feeds filter dropdowns.
func (s *store) distinctProjects() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT project FROM observations
		  WHERE deleted_at IS NULL AND project IS NOT NULL AND project != ''
		  ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- HTTP handlers ----------

func (s *server) handleStatsOverview(w http.ResponseWriter, r *http.Request) {
	o, err := s.db.statsOverview()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *server) handleStatsTimeseries(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	pts, err := s.db.statsTimeseries(days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": pts})
}

func (s *server) handleStatsBreakdown(w http.ResponseWriter, r *http.Request) {
	byType, byProject, byScope, err := s.db.statsBreakdown()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"by_type": byType, "by_project": byProject, "by_scope": byScope,
	})
}

func (s *server) handleStatsTopics(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	topics, err := s.db.statsTopics(limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"topics": topics})
}

func (s *server) handleMemories(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	page, _ := strconv.Atoi(qv.Get("page"))
	size, _ := strconv.Atoi(qv.Get("size"))
	items, total, err := s.db.listMemories(
		qv.Get("q"), qv.Get("project"), qv.Get("type"), page, size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	projects, _ := s.db.distinctProjects()
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "projects": projects,
	})
}

func (s *server) handleMemoryDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	m, err := s.db.getMemory(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "不存在或已删除")
		return
	}
	writeJSON(w, http.StatusOK, m)
}
