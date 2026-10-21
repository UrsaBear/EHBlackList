package blacklist

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blacklist.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Logf("close store: %v", err)
		}
	})
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	return store
}

var legacySchemaStatements = []string{
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

func TestLegacyDatabaseCompatibility(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range legacySchemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO blacklist_reasons(code, label, sort_order, builtin) VALUES (?, ?, ?, ?)`,
		"artist", "不喜欢作者", 10, 1,
	); err != nil {
		t.Fatal(err)
	}
	const createdAt = int64(1700000000)
	legacyEntries := []struct {
		typ, value, norm, code, label string
	}{
		{"id", "123456", "123456", "artist", "不喜欢作者"},
		{"keyword", "岡雅", "岡雅", "artist", "不喜欢作者"},
	}
	for _, e := range legacyEntries {
		if _, err := db.Exec(
			`INSERT INTO blacklist_entries(type, value, value_norm, reason_code, reason_label, priority, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.typ, e.value, e.norm, e.code, e.label, 100, createdAt, createdAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init legacy database: %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.IDs != 1 || stats.Keywords != 1 || stats.Reasons != 8 {
		t.Fatalf("unexpected stats for legacy database: %+v", stats)
	}

	results := store.Check([]CheckItem{
		{GID: "123456", Title: "anything"},
		{GID: "999", Title: "xx岡雅xx"},
		{GID: "999", Title: "no match"},
	})
	if !results[0].Blacklisted || results[0].Match == nil || results[0].Match.Type != "id" {
		t.Fatalf("legacy ID entry did not match: %+v", results[0])
	}
	if !results[1].Blacklisted || results[1].Match == nil || results[1].Match.Type != "keyword" {
		t.Fatalf("legacy keyword entry did not match: %+v", results[1])
	}
	if results[2].Blacklisted {
		t.Fatalf("unexpected match: %+v", results[2])
	}

	added, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "777", ReasonCode: "content"})
	if err != nil {
		t.Fatalf("write to legacy database: %v", err)
	}
	if added.ReasonLabel != "题材避雷" {
		t.Fatalf("reason label was not filled from the dictionary: %+v", added)
	}
	if stats, _ = store.Stats(ctx); stats.Total != 3 {
		t.Fatalf("expected 3 entries after write, got %+v", stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer reopened.Close()
	if err := reopened.Init(ctx); err != nil {
		t.Fatalf("reinit database: %v", err)
	}
	if stats, _ = reopened.Stats(ctx); stats.Total != 3 {
		t.Fatalf("entries did not persist: %+v", stats)
	}
	var storedCreatedAt int64
	if err := reopened.db.QueryRowContext(ctx,
		`SELECT created_at FROM blacklist_entries WHERE type = 'id' AND value_norm = '123456'`,
	).Scan(&storedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if storedCreatedAt != createdAt {
		t.Fatalf("legacy created_at changed: got %d, want %d", storedCreatedAt, createdAt)
	}
}

func TestExportIsComplete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5101; i++ {
		value := strconv.Itoa(100000 + i)
		err := upsertEntryExec(ctx, tx, Entry{
			Type:      "id",
			Value:     value,
			ValueNorm: value,
			Priority:  100,
			CreatedAt: 1700000000,
		})
		if err != nil {
			t.Fatalf("insert entry %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.reload(ctx); err != nil {
		t.Fatal(err)
	}

	data, err := store.Export(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if data.Version != 2 {
		t.Fatalf("version = %d, want 2", data.Version)
	}
	if len(data.Entries) != 5101 {
		t.Fatalf("export truncated: got %d entries, want 5101", len(data.Entries))
	}
	if len(data.Reasons) != 8 {
		t.Fatalf("export reasons = %d, want 8", len(data.Reasons))
	}
	if data.Exported == 0 {
		t.Fatal("export timestamp is zero")
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(body) {
		t.Fatal("export is not valid JSON")
	}
}

func TestImportLegacyAndV2(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	res, err := store.Import(ctx, []byte(`{"ids":["123","abc"],"keywords":["Foo, Bar"]}`), true)
	if err != nil {
		t.Fatalf("legacy import: %v", err)
	}
	if res.ImportedEntries != 2 || res.Skipped != 1 {
		t.Fatalf("unexpected legacy import result: %+v", res)
	}
	stats, _ := store.Stats(ctx)
	if stats.IDs != 1 || stats.Keywords != 1 {
		t.Fatalf("unexpected stats after legacy import: %+v", stats)
	}
	results := store.Check([]CheckItem{{GID: "999", Title: "foo bar"}})
	if !results[0].Blacklisted || results[0].Match.ValueNorm != "foobar" {
		t.Fatalf("legacy keyword was not normalized: %+v", results[0])
	}

	payload := ExportData{
		Version: 2,
		Reasons: []Reason{{Code: "custom-x", Label: "自定义", SortOrder: 5}},
		Entries: []Entry{{
			Type:        "keyword",
			Value:       "测试",
			ValueNorm:   "测试",
			ReasonCode:  "custom-x",
			ReasonLabel: "自定义",
			Priority:    50,
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	res, err = store.Import(ctx, body, false)
	if err != nil {
		t.Fatalf("v2 import: %v", err)
	}
	if res.ImportedEntries != 1 || res.ImportedReasons != 1 {
		t.Fatalf("unexpected v2 import result: %+v", res)
	}
	stats, _ = store.Stats(ctx)
	if stats.Keywords != 2 || stats.Reasons != 9 {
		t.Fatalf("unexpected stats after v2 import: %+v", stats)
	}

	before, _ := store.Stats(ctx)
	if _, err := store.Import(ctx, []byte(`{"entries":[{"type":"bogus","value":"x"}]}`), true); !IsValidationError(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
	after, _ := store.Stats(ctx)
	if before != after {
		t.Fatalf("failed overwrite import changed data: before=%+v after=%+v", before, after)
	}

	if _, err := store.Import(ctx, []byte(`{`), false); !IsValidationError(err) {
		t.Fatalf("expected invalid JSON validation error, got %v", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := newTestStore(t)
	if _, err := source.UpsertEntry(ctx, EntryRequest{
		Type: "keyword", Value: "秘密", ReasonCode: "content", Priority: intPtr(30),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.UpsertEntry(ctx, EntryRequest{
		Type: "id", Value: "4242", ReasonCode: "artist",
	}); err != nil {
		t.Fatal(err)
	}

	data, err := source.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}

	target := newTestStore(t)
	res, err := target.Import(ctx, body, true)
	if err != nil {
		t.Fatalf("round-trip import: %v", err)
	}
	if res.ImportedEntries != 2 {
		t.Fatalf("unexpected round-trip result: %+v", res)
	}
	results := target.Check([]CheckItem{
		{GID: "4242"},
		{GID: "1", Title: "这是秘密标题"},
	})
	if !results[0].Blacklisted || !results[1].Blacklisted {
		t.Fatalf("round-trip entries do not match: %+v", results)
	}
}

func TestReasonCascade(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	entry, err := store.UpsertEntry(ctx, EntryRequest{
		Type: "keyword", Value: "abc", ReasonCode: "artist", ReasonLabel: "旧标签",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ReasonLabel != "旧标签" {
		t.Fatalf("explicit label was overwritten: %+v", entry)
	}

	if _, err := store.UpsertReason(ctx, ReasonRequest{Code: "artist", Label: "新标签", SortOrder: intPtr(10)}); err != nil {
		t.Fatal(err)
	}
	entry, err = store.getEntry(ctx, "keyword", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ReasonLabel != "新标签" {
		t.Fatalf("reason rename did not cascade: %+v", entry)
	}

	entry, err = store.UpsertEntry(ctx, EntryRequest{
		Type: "keyword", Value: "def", ReasonCode: "content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.ReasonLabel != "题材避雷" {
		t.Fatalf("label was not filled from dictionary: %+v", entry)
	}

	if err := store.DeleteReason(ctx, "artist"); err != nil {
		t.Fatal(err)
	}
	entry, err = store.getEntry(ctx, "keyword", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ReasonCode != "" {
		t.Fatalf("reason_code was not cleared: %+v", entry)
	}
	if entry.ReasonLabel != "新标签" {
		t.Fatalf("historical label was not preserved: %+v", entry)
	}
	if err := store.DeleteReason(ctx, "artist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestImportCascadesReasonRename(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.UpsertEntry(ctx, EntryRequest{
		Type: "keyword", Value: "abc", ReasonCode: "custom-x", ReasonLabel: "旧标签",
	}); err != nil {
		t.Fatal(err)
	}

	payload := ExportData{
		Version: 2,
		Reasons: []Reason{{Code: "custom-x", Label: "新标签", SortOrder: 5}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import(ctx, body, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	entry, err := store.getEntry(ctx, "keyword", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ReasonLabel != "新标签" {
		t.Fatalf("import did not cascade reason label: %+v", entry)
	}
}

func TestDeleteAndListEntries(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "42"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "keyword", Value: "Foo Bar"}); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ListEntries(ctx, "keyword", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ValueNorm != "foobar" {
		t.Fatalf("unexpected keyword list: %+v", entries)
	}
	entries, err = store.ListEntries(ctx, "", "foo", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("search returned %d entries, want 1", len(entries))
	}
	if _, err := store.ListEntries(ctx, "bogus", "", 10, 0); !IsValidationError(err) {
		t.Fatalf("expected validation error for bad type, got %v", err)
	}

	if err := store.DeleteEntry(ctx, "keyword", "foobar"); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	if err := store.DeleteEntry(ctx, "keyword", "foobar"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if err := store.DeleteEntry(ctx, "bogus", "x"); !IsValidationError(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestEntryTimestamps(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if first.CreatedAt == 0 || first.UpdatedAt == 0 {
		t.Fatalf("timestamps were not populated: %+v", first)
	}

	time.Sleep(1100 * time.Millisecond)
	second, err := store.UpsertEntry(ctx, EntryRequest{
		Type: "id", Value: "5", ReasonCode: "other", Priority: intPtr(5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.CreatedAt != first.CreatedAt {
		t.Fatalf("created_at changed on update: %d -> %d", first.CreatedAt, second.CreatedAt)
	}
	if second.UpdatedAt <= first.UpdatedAt {
		t.Fatalf("updated_at did not advance: %d -> %d", first.UpdatedAt, second.UpdatedAt)
	}
	if second.Priority != 5 || second.ReasonLabel != "其他" {
		t.Fatalf("update did not persist fields: %+v", second)
	}
}

func TestReasonLabelUniqueness(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertReason(ctx, ReasonRequest{Code: "custom-a", Label: "自定义"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertReason(ctx, ReasonRequest{Code: "custom-b", Label: "自定义"}); err == nil {
		t.Fatal("expected duplicate label to be rejected")
	}
}

func TestOverwriteImport(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "1"}); err != nil {
		t.Fatal(err)
	}

	payload := `{"ids":["2"],"keywords":["` + strings.Repeat("x", 10) + `"]}`
	if _, err := store.Import(ctx, []byte(payload), true); err != nil {
		t.Fatal(err)
	}
	stats, _ := store.Stats(ctx)
	if stats.Total != 2 {
		t.Fatalf("unexpected stats after overwrite import: %+v", stats)
	}
}
