package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const (
	defaultAddr   = ":8787"
	defaultDBPath = "./data/blacklist.db"
	defaultLimit  = 200
	maxLimit      = 5000
	defaultPrio   = 100
)

type App struct {
	db *sql.DB

	mu             sync.RWMutex
	idIndex        map[string]Entry
	keywordEntries []Entry
}

type Entry struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueNorm   string `json:"value_norm"`
	ReasonCode  string `json:"reason_code,omitempty"`
	ReasonLabel string `json:"reason_label,omitempty"`
	Priority    int    `json:"priority"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type EntryRequest struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueNorm   string `json:"value_norm,omitempty"`
	ReasonCode  string `json:"reason_code,omitempty"`
	ReasonLabel string `json:"reason_label,omitempty"`
	Priority    *int   `json:"priority,omitempty"`
}

type Reason struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	SortOrder int    `json:"sort_order"`
	Builtin   bool   `json:"builtin"`
}

type ReasonRequest struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	SortOrder *int   `json:"sort_order,omitempty"`
	Builtin   *bool  `json:"builtin,omitempty"`
}

type CheckRequest struct {
	Items []CheckItem `json:"items"`
}

type CheckItem struct {
	GID   string `json:"gid"`
	Title string `json:"title"`
}

type CheckResponse struct {
	Items []CheckResult `json:"items"`
}

type CheckResult struct {
	GID         string       `json:"gid"`
	Blacklisted bool         `json:"blacklisted"`
	Match       *MatchResult `json:"match"`
}

type MatchResult struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueNorm   string `json:"value_norm"`
	ReasonCode  string `json:"reason_code,omitempty"`
	ReasonLabel string `json:"reason_label,omitempty"`
	Priority    int    `json:"priority"`
}

type ExportData struct {
	Version  int      `json:"version"`
	Reasons  []Reason `json:"reasons"`
	Entries  []Entry  `json:"entries"`
	Exported int64    `json:"exported_at"`
}

type LegacyImport struct {
	IDs      []string `json:"ids"`
	Keywords []string `json:"keywords"`
}

type Stats struct {
	IDs      int `json:"ids"`
	Keywords int `json:"keywords"`
	Reasons  int `json:"reasons"`
	Total    int `json:"total"`
}

type APIError struct {
	Error string `json:"error"`
}

func main() {
	addr := getenv("ADDR", defaultAddr)
	dbPath := getenv("DB_PATH", defaultDBPath)

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("create data dir failed: %v", err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		log.Fatalf("open db failed: %v", err)
	}
	defer db.Close()

	app := &App{db: db}
	if err := app.initSchema(context.Background()); err != nil {
		log.Fatalf("init schema failed: %v", err)
	}
	if err := app.seedDefaultReasons(context.Background()); err != nil {
		log.Fatalf("seed reasons failed: %v", err)
	}
	if err := app.ReloadIndex(context.Background()); err != nil {
		log.Fatalf("load index failed: %v", err)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)

	server := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(corsMiddleware(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("EH blacklist API listening on %s", addr)
	log.Printf("DB path: %s", dbPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func openDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, q := range pragmas {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	return db, nil
}

func (a *App) initSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS blacklist_entries (
			type TEXT NOT NULL CHECK (type IN ('id', 'keyword')),
			value TEXT NOT NULL,
			value_norm TEXT NOT NULL,
			reason_code TEXT,
			reason_label TEXT NOT NULL DEFAULT '',
			priority INTEGER NOT NULL DEFAULT 100,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (type, value_norm)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_blacklist_entries_type_priority
			ON blacklist_entries(type, priority, created_at, value_norm);`,
		`CREATE TABLE IF NOT EXISTS blacklist_reasons (
			code TEXT PRIMARY KEY,
			label TEXT NOT NULL UNIQUE,
			sort_order INTEGER NOT NULL DEFAULT 100,
			builtin INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS idx_blacklist_reasons_sort
			ON blacklist_reasons(sort_order, label);`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) seedDefaultReasons(ctx context.Context) error {
	defaults := []Reason{
		{Code: "artist", Label: "不喜欢作者", SortOrder: 10, Builtin: true},
		{Code: "content", Label: "题材避雷", SortOrder: 20, Builtin: true},
		{Code: "style", Label: "画风不合", SortOrder: 30, Builtin: true},
		{Code: "quality", Label: "质量太低", SortOrder: 40, Builtin: true},
		{Code: "duplicate", Label: "重复/合集", SortOrder: 50, Builtin: true},
		{Code: "language", Label: "语言/翻译问题", SortOrder: 60, Builtin: true},
		{Code: "hardcore", Label: "重口", SortOrder: 70, Builtin: true},
		{Code: "other", Label: "其他", SortOrder: 999, Builtin: true},
	}
	for _, r := range defaults {
		_, err := a.db.ExecContext(ctx,
			`INSERT INTO blacklist_reasons(code, label, sort_order, builtin)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(code) DO NOTHING`,
			r.Code, r.Label, r.SortOrder, boolToInt(r.Builtin),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) ReloadIndex(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx,
		`SELECT type, value, value_norm, COALESCE(reason_code, ''), reason_label, priority, created_at, updated_at
		 FROM blacklist_entries
		 ORDER BY type, priority ASC, created_at ASC, value_norm ASC`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	idIndex := make(map[string]Entry)
	keywords := make([]Entry, 0)

	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Type, &e.Value, &e.ValueNorm, &e.ReasonCode, &e.ReasonLabel, &e.Priority, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return err
		}
		if e.Type == "id" {
			idIndex[e.ValueNorm] = e
		} else if e.Type == "keyword" {
			keywords = append(keywords, e)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	sort.SliceStable(keywords, func(i, j int) bool {
		if keywords[i].Priority != keywords[j].Priority {
			return keywords[i].Priority < keywords[j].Priority
		}
		if keywords[i].CreatedAt != keywords[j].CreatedAt {
			return keywords[i].CreatedAt < keywords[j].CreatedAt
		}
		return keywords[i].ValueNorm < keywords[j].ValueNorm
	})

	a.mu.Lock()
	a.idIndex = idIndex
	a.keywordEntries = keywords
	a.mu.Unlock()

	return nil
}

func (a *App) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/stats", a.handleStats)
	mux.HandleFunc("/check", a.handleCheck)
	mux.HandleFunc("/entries", a.handleEntries)
	mux.HandleFunc("/entries/", a.handleEntryByPath)
	mux.HandleFunc("/reasons", a.handleReasons)
	mux.HandleFunc("/reasons/", a.handleReasonByPath)
	mux.HandleFunc("/export", a.handleExport)
	mux.HandleFunc("/import", a.handleImport)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().Unix()})
}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	stats, err := a.getStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *App) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	var req CheckRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	resp := CheckResponse{Items: make([]CheckResult, 0, len(req.Items))}
	for _, item := range req.Items {
		resp.Items = append(resp.Items, a.CheckOne(item))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) CheckOne(item CheckItem) CheckResult {
	gid := strings.TrimSpace(item.GID)

	a.mu.RLock()
	defer a.mu.RUnlock()

	if gid != "" {
		if e, ok := a.idIndex[gid]; ok {
			return CheckResult{GID: item.GID, Blacklisted: true, Match: entryToMatch(e)}
		}
	}

	normalizedTitle := normalizeText(item.Title)
	if normalizedTitle != "" {
		for _, e := range a.keywordEntries {
			if e.ValueNorm == "" {
				continue
			}
			if strings.Contains(normalizedTitle, e.ValueNorm) {
				return CheckResult{GID: item.GID, Blacklisted: true, Match: entryToMatch(e)}
			}
		}
	}

	return CheckResult{GID: item.GID, Blacklisted: false, Match: nil}
}

func (a *App) handleEntries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listEntries(w, r)
	case http.MethodPost, http.MethodPut:
		a.upsertEntryFromBody(w, r)
	case http.MethodDelete:
		a.deleteEntryFromRequest(w, r)
	default:
		writeMethodNotAllowed(w)
	}
}

func (a *App) handleEntryByPath(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/entries/"))
	if len(parts) < 2 {
		writeErrorMessage(w, http.StatusBadRequest, "expected /entries/{type}/{value_norm}")
		return
	}
	typ := parts[0]
	valueNorm, err := url.PathUnescape(parts[1])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := a.deleteEntry(r.Context(), typ, valueNorm); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.ReloadIndex(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	case http.MethodPut:
		var req EntryRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Type == "" {
			req.Type = typ
		}
		if req.ValueNorm == "" {
			req.ValueNorm = valueNorm
		}
		entry, err := normalizeEntryRequest(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if entry.Type != typ || entry.ValueNorm != valueNorm {
			writeErrorMessage(w, http.StatusBadRequest, "path type/value_norm and body type/value_norm do not match")
			return
		}
		if err := a.upsertEntry(r.Context(), entry); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err := a.ReloadIndex(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	default:
		writeMethodNotAllowed(w)
	}
}

func (a *App) listEntries(w http.ResponseWriter, r *http.Request) {
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := parseIntDefault(r.URL.Query().Get("limit"), defaultLimit)
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}
	if offset < 0 {
		offset = 0
	}

	entries, err := a.queryEntries(r.Context(), typ, q, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": entries, "limit": limit, "offset": offset})
}

func (a *App) upsertEntryFromBody(w http.ResponseWriter, r *http.Request) {
	var req EntryRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	entry, err := normalizeEntryRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.upsertEntry(r.Context(), entry); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.ReloadIndex(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (a *App) deleteEntryFromRequest(w http.ResponseWriter, r *http.Request) {
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	valueNorm := strings.TrimSpace(r.URL.Query().Get("value_norm"))
	value := strings.TrimSpace(r.URL.Query().Get("value"))

	if valueNorm == "" && value != "" {
		var err error
		valueNorm, err = normalizeValue(typ, value)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := a.deleteEntry(r.Context(), typ, valueNorm); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := a.ReloadIndex(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (a *App) queryEntries(ctx context.Context, typ, q string, limit, offset int) ([]Entry, error) {
	args := make([]any, 0)
	where := make([]string, 0)
	if typ != "" {
		if typ != "id" && typ != "keyword" {
			return nil, fmt.Errorf("invalid type: %s", typ)
		}
		where = append(where, "type = ?")
		args = append(args, typ)
	}
	if q != "" {
		where = append(where, "(value LIKE ? OR value_norm LIKE ? OR reason_label LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	query := `SELECT type, value, value_norm, COALESCE(reason_code, ''), reason_label, priority, created_at, updated_at FROM blacklist_entries`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY type ASC, priority ASC, created_at ASC, value_norm ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]Entry, 0)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Type, &e.Value, &e.ValueNorm, &e.ReasonCode, &e.ReasonLabel, &e.Priority, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (a *App) upsertEntry(ctx context.Context, e Entry) error {
	now := time.Now().Unix()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO blacklist_entries(type, value, value_norm, reason_code, reason_label, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(type, value_norm) DO UPDATE SET
			value = excluded.value,
			reason_code = excluded.reason_code,
			reason_label = excluded.reason_label,
			priority = excluded.priority,
			updated_at = excluded.updated_at`,
		e.Type, e.Value, e.ValueNorm, nullEmpty(e.ReasonCode), e.ReasonLabel, e.Priority, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (a *App) deleteEntry(ctx context.Context, typ, valueNorm string) error {
	if typ != "id" && typ != "keyword" {
		return fmt.Errorf("invalid type: %s", typ)
	}
	if strings.TrimSpace(valueNorm) == "" {
		return errors.New("value_norm is required")
	}
	res, err := a.db.ExecContext(ctx, `DELETE FROM blacklist_entries WHERE type = ? AND value_norm = ?`, typ, valueNorm)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("entry not found")
	}
	return nil
}

func (a *App) handleReasons(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		reasons, err := a.queryReasons(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": reasons})
	case http.MethodPost:
		var req ReasonRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		reason, err := normalizeReasonRequest(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.upsertReason(r.Context(), reason); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, reason)
	default:
		writeMethodNotAllowed(w)
	}
}

func (a *App) handleReasonByPath(w http.ResponseWriter, r *http.Request) {
	code, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/reasons/"))
	if err != nil || strings.TrimSpace(code) == "" {
		writeErrorMessage(w, http.StatusBadRequest, "invalid reason code")
		return
	}
	code = normalizeCode(code)

	switch r.Method {
	case http.MethodPut:
		var req ReasonRequest
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.Code = code
		reason, err := normalizeReasonRequest(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := a.upsertReason(r.Context(), reason); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, reason)
	case http.MethodDelete:
		if err := a.deleteReason(r.Context(), code); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	default:
		writeMethodNotAllowed(w)
	}
}

func (a *App) queryReasons(ctx context.Context) ([]Reason, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT code, label, sort_order, builtin FROM blacklist_reasons ORDER BY sort_order ASC, label ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reasons := make([]Reason, 0)
	for rows.Next() {
		var r Reason
		var builtin int
		if err := rows.Scan(&r.Code, &r.Label, &r.SortOrder, &builtin); err != nil {
			return nil, err
		}
		r.Builtin = builtin != 0
		reasons = append(reasons, r)
	}
	return reasons, rows.Err()
}

func (a *App) upsertReason(ctx context.Context, r Reason) error {
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO blacklist_reasons(code, label, sort_order, builtin)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(code) DO UPDATE SET
			label = excluded.label,
			sort_order = excluded.sort_order,
			builtin = excluded.builtin`,
		r.Code, r.Label, r.SortOrder, boolToInt(r.Builtin),
	)
	return err
}

func (a *App) deleteReason(ctx context.Context, code string) error {
	res, err := a.db.ExecContext(ctx, `DELETE FROM blacklist_reasons WHERE code = ?`, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("reason not found")
	}
	return nil
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	entries, err := a.queryEntries(r.Context(), "", "", maxLimit, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reasons, err := a.queryReasons(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, ExportData{Version: 2, Reasons: reasons, Entries: entries, Exported: time.Now().Unix()})
}

func (a *App) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeMethodNotAllowed(w)
		return
	}

	var raw map[string]json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	overwrite := parseBoolDefault(r.URL.Query().Get("overwrite"), true)

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if overwrite {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM blacklist_entries`); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	importedEntries := 0
	importedReasons := 0

	if _, ok := raw["entries"]; ok {
		var data ExportData
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &data); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		for _, reason := range data.Reasons {
			normReason, err := normalizeReasonRequest(ReasonRequest{
				Code:      reason.Code,
				Label:     reason.Label,
				SortOrder: &reason.SortOrder,
				Builtin:   &reason.Builtin,
			})
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if _, err := tx.ExecContext(r.Context(),
				`INSERT INTO blacklist_reasons(code, label, sort_order, builtin)
				 VALUES (?, ?, ?, ?)
				 ON CONFLICT(code) DO UPDATE SET label = excluded.label, sort_order = excluded.sort_order, builtin = excluded.builtin`,
				normReason.Code, normReason.Label, normReason.SortOrder, boolToInt(normReason.Builtin),
			); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			importedReasons++
		}

		for _, entry := range data.Entries {
			prio := entry.Priority
			req := EntryRequest{
				Type:        entry.Type,
				Value:       entry.Value,
				ValueNorm:   entry.ValueNorm,
				ReasonCode:  entry.ReasonCode,
				ReasonLabel: entry.ReasonLabel,
				Priority:    &prio,
			}
			normEntry, err := normalizeEntryRequest(req)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := upsertEntryTx(r.Context(), tx, normEntry); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			importedEntries++
		}
	} else {
		var legacy LegacyImport
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &legacy); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		for _, id := range legacy.IDs {
			prio := defaultPrio
			entry, err := normalizeEntryRequest(EntryRequest{Type: "id", Value: id, Priority: &prio})
			if err != nil {
				continue
			}
			if err := upsertEntryTx(r.Context(), tx, entry); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			importedEntries++
		}
		for _, kw := range legacy.Keywords {
			prio := defaultPrio
			entry, err := normalizeEntryRequest(EntryRequest{Type: "keyword", Value: kw, Priority: &prio})
			if err != nil {
				continue
			}
			if err := upsertEntryTx(r.Context(), tx, entry); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			importedEntries++
		}
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.ReloadIndex(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported_entries": importedEntries,
		"imported_reasons": importedReasons,
		"overwrite":        overwrite,
	})
}

func upsertEntryTx(ctx context.Context, tx *sql.Tx, e Entry) error {
	now := time.Now().Unix()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	_, err := tx.ExecContext(ctx,
		`INSERT INTO blacklist_entries(type, value, value_norm, reason_code, reason_label, priority, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(type, value_norm) DO UPDATE SET
			value = excluded.value,
			reason_code = excluded.reason_code,
			reason_label = excluded.reason_label,
			priority = excluded.priority,
			updated_at = excluded.updated_at`,
		e.Type, e.Value, e.ValueNorm, nullEmpty(e.ReasonCode), e.ReasonLabel, e.Priority, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

func (a *App) getStats(ctx context.Context) (Stats, error) {
	var s Stats
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_entries WHERE type = 'id'`).Scan(&s.IDs); err != nil {
		return s, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_entries WHERE type = 'keyword'`).Scan(&s.Keywords); err != nil {
		return s, err
	}
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_reasons`).Scan(&s.Reasons); err != nil {
		return s, err
	}
	s.Total = s.IDs + s.Keywords
	return s, nil
}

func normalizeEntryRequest(req EntryRequest) (Entry, error) {
	typ := strings.TrimSpace(req.Type)
	if typ != "id" && typ != "keyword" {
		return Entry{}, fmt.Errorf("invalid type: %s", typ)
	}

	value := strings.TrimSpace(req.Value)
	valueNorm := strings.TrimSpace(req.ValueNorm)
	if valueNorm == "" {
		var err error
		valueNorm, err = normalizeValue(typ, value)
		if err != nil {
			return Entry{}, err
		}
	}
	if value == "" {
		value = valueNorm
	}

	priority := defaultPrio
	if req.Priority != nil {
		priority = *req.Priority
	}

	return Entry{
		Type:        typ,
		Value:       value,
		ValueNorm:   valueNorm,
		ReasonCode:  normalizeCode(req.ReasonCode),
		ReasonLabel: trimReason(req.ReasonLabel),
		Priority:    priority,
	}, nil
}

func normalizeReasonRequest(req ReasonRequest) (Reason, error) {
	code := normalizeCode(req.Code)
	label := strings.TrimSpace(req.Label)
	if label == "" {
		return Reason{}, errors.New("reason label is required")
	}
	if code == "" {
		code = "custom-" + shortHash(label)
	}
	sortOrder := 100
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}
	builtin := false
	if req.Builtin != nil {
		builtin = *req.Builtin
	}
	return Reason{Code: code, Label: trimReason(label), SortOrder: sortOrder, Builtin: builtin}, nil
}

func normalizeValue(typ, value string) (string, error) {
	value = strings.TrimSpace(value)
	if typ == "id" {
		if value == "" {
			return "", errors.New("id value is required")
		}
		for _, r := range value {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("invalid id: %s", value)
			}
		}
		return value, nil
	}
	if typ == "keyword" {
		norm := normalizeText(value)
		if norm == "" {
			return "", errors.New("keyword is empty after normalization")
		}
		return norm, nil
	}
	return "", fmt.Errorf("invalid type: %s", typ)
}

func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

func normalizeText(str string) string {
	if str == "" {
		return ""
	}
	s := strings.ToLower(str)

	punctMap := map[rune]rune{
		'，': ',',
		'。': '.',
		'！': '!',
		'？': '?',
		'：': ':',
		'；': ';',
		'（': '(',
		'）': ')',
		'【': '[',
		'】': ']',
		'「': '"',
		'」': '"',
		'『': '"',
		'』': '"',
		'—': '-',
		'－': '-',
		'～': '~',
		'·': '.',
		'、': ',',
		'《': '<',
		'》': '>',
	}

	var b strings.Builder
	for _, r := range s {
		if mapped, ok := punctMap[r]; ok {
			r = mapped
		}
		if shouldRemoveRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func shouldRemoveRune(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	if strings.ContainsRune("\t\n\r\f\v.,!?-_:;'\"`~[](){}<>/\\", r) {
		return true
	}
	if strings.ContainsRune("，。？！：；…【】「」『』、《》、（）", r) {
		return true
	}
	return false
}

func entryToMatch(e Entry) *MatchResult {
	return &MatchResult{
		Type:        e.Type,
		Value:       e.Value,
		ValueNorm:   e.ValueNorm,
		ReasonCode:  e.ReasonCode,
		ReasonLabel: e.ReasonLabel,
		Priority:    e.Priority,
	}
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, APIError{Error: err.Error()})
}

func writeErrorMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, APIError{Error: msg})
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeErrorMessage(w, http.StatusMethodNotAllowed, "method not allowed")
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w}

		defer func() {
			status := lw.status
			if status == 0 {
				status = http.StatusOK
			}
			if recovered := recover(); recovered != nil {
				if lw.status == 0 {
					status = http.StatusInternalServerError
				}
				log.Printf(
					"http_request method=%s path=%q query=%q status=%d bytes=%d duration=%s remote=%q user_agent=%q panic=%q",
					r.Method,
					r.URL.Path,
					r.URL.RawQuery,
					status,
					lw.bytes,
					time.Since(start).Round(time.Microsecond),
					clientIP(r),
					r.UserAgent(),
					fmt.Sprint(recovered),
				)
				panic(recovered)
			}

			log.Printf(
				"http_request method=%s path=%q query=%q status=%d bytes=%d duration=%s remote=%q user_agent=%q",
				r.Method,
				r.URL.Path,
				r.URL.RawQuery,
				status,
				lw.bytes,
				time.Since(start).Round(time.Microsecond),
				clientIP(r),
				r.UserAgent(),
			)
		}()

		next.ServeHTTP(lw, r)
	})
}

func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func trimReason(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 40 {
		return string(runes[:40])
	}
	return s
}

func normalizeCode(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func nullEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func parseIntDefault(s string, def int) int {
	if strings.TrimSpace(s) == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func parseBoolDefault(s string, def bool) bool {
	if strings.TrimSpace(s) == "" {
		return def
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return def
	}
	return v
}

func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func splitPath(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
