package blacklist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested entry or reason does not exist.
var ErrNotFound = errors.New("not found")

const entryColumns = `type, value, value_norm, COALESCE(reason_code, ''), reason_label, priority, created_at, updated_at`

// Store owns the SQLite database and the in-memory index. All mutating
// operations are serialized; reads of the index only take a short RWMutex.
type Store struct {
	db *sql.DB

	writeMu sync.Mutex
	mu      sync.RWMutex
	idx     *index
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type rowScanner interface {
	Scan(dest ...any) error
}

// OpenStore opens (and creates if needed) the SQLite database at dbPath. The
// on-disk schema is identical to the original single-file server, so an
// existing blacklist.db keeps working without a migration step.
func OpenStore(dbPath string) (*Store, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, errors.New("database path is required")
	}
	if dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:") {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA foreign_keys=ON;",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return &Store{db: db}, nil
}

// Init creates the schema, seeds built-in reasons and loads the index. It is
// safe to run against a database created by the original server.
func (s *Store) Init(ctx context.Context) error {
	if err := s.initSchema(ctx); err != nil {
		return err
	}
	if err := s.seedDefaultReasons(ctx); err != nil {
		return err
	}
	return s.reload(ctx)
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) initSchema(ctx context.Context) error {
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
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) seedDefaultReasons(ctx context.Context) error {
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
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO blacklist_reasons(code, label, sort_order, builtin)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(code) DO NOTHING`,
			r.Code, r.Label, r.SortOrder, boolToInt(r.Builtin),
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+`
		 FROM blacklist_entries
		 ORDER BY type, priority ASC, created_at ASC, value_norm ASC`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	entries := make([]Entry, 0, 128)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.replaceIndex(entries)
	return nil
}

func (s *Store) replaceIndex(entries []Entry) {
	s.mu.Lock()
	s.idx = newIndex(entries)
	s.mu.Unlock()
}

// Check evaluates a batch against a consistent in-memory snapshot.
func (s *Store) Check(items []CheckItem) []CheckResult {
	s.mu.RLock()
	ix := s.idx
	s.mu.RUnlock()

	results := make([]CheckResult, 0, len(items))
	for _, item := range items {
		gid := truncateRunes(strings.TrimSpace(item.GID), maxGIDRunes)
		title := truncateRunes(item.Title, maxTitleRunes)
		if ix != nil {
			if e, ok := ix.match(gid, title); ok {
				results = append(results, CheckResult{
					GID:         item.GID,
					Blacklisted: true,
					Match:       entryToMatch(e),
				})
				continue
			}
		}
		results = append(results, CheckResult{GID: item.GID, Blacklisted: false})
	}
	return results
}

// ListEntries returns one page of entries.
func (s *Store) ListEntries(ctx context.Context, typ, filter string, limit, offset int) ([]Entry, error) {
	args := make([]any, 0, 5)
	where := make([]string, 0, 2)
	if typ != "" {
		if typ != "id" && typ != "keyword" {
			return nil, invalidf("invalid type: %s", typ)
		}
		where = append(where, "type = ?")
		args = append(args, typ)
	}
	if filter != "" {
		where = append(where, "(value LIKE ? OR value_norm LIKE ? OR reason_label LIKE ?)")
		like := "%" + filter + "%"
		args = append(args, like, like, like)
	}

	query := `SELECT ` + entryColumns + ` FROM blacklist_entries`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY type ASC, priority ASC, created_at ASC, value_norm ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]Entry, 0, limit)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Stats returns entry and reason counts.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_entries WHERE type = 'id'`).Scan(&st.IDs); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_entries WHERE type = 'keyword'`).Scan(&st.Keywords); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blacklist_reasons`).Scan(&st.Reasons); err != nil {
		return st, err
	}
	st.Total = st.IDs + st.Keywords
	return st, nil
}

// UpsertEntry normalizes and stores a rule, filling in the reason label from
// the dictionary when the caller omitted it.
func (s *Store) UpsertEntry(ctx context.Context, req EntryRequest) (Entry, error) {
	e, err := normalizeEntryRequest(req)
	if err != nil {
		return Entry{}, err
	}
	return s.UpsertEntryNormalized(ctx, e)
}

// UpsertEntryNormalized stores an already-normalized entry.
func (s *Store) UpsertEntryNormalized(ctx context.Context, e Entry) (Entry, error) {
	if e.Type != "id" && e.Type != "keyword" {
		return Entry{}, invalidf("invalid type: %s", e.Type)
	}
	if e.ValueNorm == "" {
		return Entry{}, invalidf("value_norm is required")
	}
	if e.Value == "" {
		e.Value = e.ValueNorm
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if e.ReasonCode != "" && e.ReasonLabel == "" {
		if label, err := s.lookupReasonLabel(ctx, e.ReasonCode); err == nil {
			e.ReasonLabel = label
		}
	}
	if err := s.upsertEntry(ctx, e); err != nil {
		return Entry{}, err
	}
	if err := s.reload(ctx); err != nil {
		return Entry{}, err
	}
	return s.getEntry(ctx, e.Type, e.ValueNorm)
}

// DeleteEntry removes a rule by its normalized value.
func (s *Store) DeleteEntry(ctx context.Context, typ, valueNorm string) error {
	if typ != "id" && typ != "keyword" {
		return invalidf("invalid type: %s", typ)
	}
	if strings.TrimSpace(valueNorm) == "" {
		return invalidf("value_norm is required")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM blacklist_entries WHERE type = ? AND value_norm = ?`, typ, valueNorm)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("entry %s/%s: %w", typ, valueNorm, ErrNotFound)
	}
	return s.reload(ctx)
}

// ListReasons returns the reason dictionary in display order.
func (s *Store) ListReasons(ctx context.Context) ([]Reason, error) {
	return queryReasonsFrom(ctx, s.db)
}

// UpsertReason creates or updates a reason. Renaming a reason also updates the
// denormalized label on every entry that references it, so old entries never
// show a stale label.
func (s *Store) UpsertReason(ctx context.Context, req ReasonRequest) (Reason, error) {
	r, err := normalizeReasonRequest(req)
	if err != nil {
		return Reason{}, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Reason{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := upsertReasonExec(ctx, tx, r); err != nil {
		return Reason{}, err
	}
	if err := cascadeReasonLabel(ctx, tx, r.Code, r.Label, time.Now().Unix()); err != nil {
		return Reason{}, err
	}
	if err := tx.Commit(); err != nil {
		return Reason{}, err
	}
	if err := s.reload(ctx); err != nil {
		return Reason{}, err
	}
	return r, nil
}

// DeleteReason removes a reason from the dictionary. Entries referencing it
// keep their last label as a historical snapshot but lose the code.
func (s *Store) DeleteReason(ctx context.Context, code string) error {
	code = normalizeCode(code)
	if code == "" {
		return invalidf("reason code is required")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE blacklist_entries SET reason_code = NULL, updated_at = ? WHERE reason_code = ?`,
		time.Now().Unix(), code,
	); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM blacklist_reasons WHERE code = ?`, code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("reason %s: %w", code, ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reload(ctx)
}

// Export returns the complete blacklist. Unlike the old implementation it is
// not capped at MaxLimit, so backups never silently lose entries.
func (s *Store) Export(ctx context.Context) (ExportData, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ExportData{}, err
	}
	defer func() { _ = tx.Rollback() }()

	reasons, err := queryReasonsFrom(ctx, tx)
	if err != nil {
		return ExportData{}, err
	}
	entries, err := queryAllEntries(ctx, tx)
	if err != nil {
		return ExportData{}, err
	}
	return ExportData{
		Version:  2,
		Reasons:  reasons,
		Entries:  entries,
		Exported: time.Now().Unix(),
	}, nil
}

// Import accepts either the version 2 export format or the legacy
// {ids, keywords} userscript format. overwrite=true replaces all entries
// inside the same transaction, so a malformed payload cannot destroy data.
func (s *Store) Import(ctx context.Context, body []byte, overwrite bool) (ImportResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ImportResult{}, invalidf("invalid JSON: %v", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if overwrite {
		if _, err := tx.ExecContext(ctx, `DELETE FROM blacklist_entries`); err != nil {
			return ImportResult{}, err
		}
	}

	res := ImportResult{Overwrite: overwrite}
	now := time.Now().Unix()
	if _, ok := raw["entries"]; ok {
		var data ExportData
		if err := json.Unmarshal(body, &data); err != nil {
			return ImportResult{}, invalidf("invalid export payload: %v", err)
		}
		for _, reason := range data.Reasons {
			r, err := normalizeReasonRequest(ReasonRequest{
				Code:      reason.Code,
				Label:     reason.Label,
				SortOrder: &reason.SortOrder,
				Builtin:   &reason.Builtin,
			})
			if err != nil {
				return ImportResult{}, fmt.Errorf("reason %q: %w", reason.Code, err)
			}
			if err := upsertReasonExec(ctx, tx, r); err != nil {
				return ImportResult{}, err
			}
			if err := cascadeReasonLabel(ctx, tx, r.Code, r.Label, now); err != nil {
				return ImportResult{}, err
			}
			res.ImportedReasons++
		}
		for _, entry := range data.Entries {
			prio := entry.Priority
			e, err := normalizeEntryRequest(EntryRequest{
				Type:        entry.Type,
				Value:       entry.Value,
				ValueNorm:   entry.ValueNorm,
				ReasonCode:  entry.ReasonCode,
				ReasonLabel: entry.ReasonLabel,
				Priority:    &prio,
			})
			if err != nil {
				return ImportResult{}, fmt.Errorf("entry %q: %w", entry.Value, err)
			}
			if e.ReasonCode != "" && e.ReasonLabel == "" {
				var label string
				if err := tx.QueryRowContext(ctx, `SELECT label FROM blacklist_reasons WHERE code = ?`, e.ReasonCode).Scan(&label); err == nil {
					e.ReasonLabel = label
				}
			}
			if err := upsertEntryExec(ctx, tx, e); err != nil {
				return ImportResult{}, err
			}
			res.ImportedEntries++
		}
	} else {
		var legacy LegacyImport
		if err := json.Unmarshal(body, &legacy); err != nil {
			return ImportResult{}, invalidf("invalid legacy payload: %v", err)
		}
		for _, id := range legacy.IDs {
			prio := DefaultPriority
			e, err := normalizeEntryRequest(EntryRequest{Type: "id", Value: id, Priority: &prio})
			if err != nil {
				res.Skipped++
				continue
			}
			if err := upsertEntryExec(ctx, tx, e); err != nil {
				return ImportResult{}, err
			}
			res.ImportedEntries++
		}
		for _, kw := range legacy.Keywords {
			prio := DefaultPriority
			e, err := normalizeEntryRequest(EntryRequest{Type: "keyword", Value: kw, Priority: &prio})
			if err != nil {
				res.Skipped++
				continue
			}
			if err := upsertEntryExec(ctx, tx, e); err != nil {
				return ImportResult{}, err
			}
			res.ImportedEntries++
		}
	}

	if err := tx.Commit(); err != nil {
		return ImportResult{}, err
	}
	if err := s.reload(ctx); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

func (s *Store) upsertEntry(ctx context.Context, e Entry) error {
	return upsertEntryExec(ctx, s.db, e)
}

func upsertEntryExec(ctx context.Context, ex execer, e Entry) error {
	now := time.Now().Unix()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	_, err := ex.ExecContext(ctx,
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

func upsertReasonExec(ctx context.Context, ex execer, r Reason) error {
	_, err := ex.ExecContext(ctx,
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

// cascadeReasonLabel keeps the denormalized label on entries in sync when a
// reason is renamed through the dictionary or an import.
func cascadeReasonLabel(ctx context.Context, ex execer, code, label string, now int64) error {
	_, err := ex.ExecContext(ctx,
		`UPDATE blacklist_entries
		 SET reason_label = ?, updated_at = ?
		 WHERE reason_code = ? AND reason_label <> ?`,
		label, now, code, label,
	)
	return err
}

func (s *Store) getEntry(ctx context.Context, typ, valueNorm string) (Entry, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entryColumns+` FROM blacklist_entries WHERE type = ? AND value_norm = ?`,
		typ, valueNorm,
	)
	return scanEntry(row)
}

func (s *Store) lookupReasonLabel(ctx context.Context, code string) (string, error) {
	var label string
	err := s.db.QueryRowContext(ctx, `SELECT label FROM blacklist_reasons WHERE code = ?`, code).Scan(&label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return label, err
}

func queryAllEntries(ctx context.Context, q queryer) ([]Entry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+entryColumns+`
		 FROM blacklist_entries
		 ORDER BY type ASC, priority ASC, created_at ASC, value_norm ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]Entry, 0, 256)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func queryReasonsFrom(ctx context.Context, q queryer) ([]Reason, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT code, label, sort_order, builtin
		 FROM blacklist_reasons
		 ORDER BY sort_order ASC, label ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reasons := make([]Reason, 0, 8)
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

func scanEntry(row rowScanner) (Entry, error) {
	var e Entry
	err := row.Scan(
		&e.Type, &e.Value, &e.ValueNorm, &e.ReasonCode, &e.ReasonLabel,
		&e.Priority, &e.CreatedAt, &e.UpdatedAt,
	)
	return e, err
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
