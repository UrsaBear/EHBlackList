package blacklist

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newHTTPHandler(t *testing.T, cfg Config) (*Store, http.Handler) {
	t.Helper()
	store := newTestStore(t)
	return store, NewServer(store, cfg).Handler()
}

func doRequest(t *testing.T, handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestAuth(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{APIToken: "secret"})

	if rec := doRequest(t, handler, http.MethodGet, "/health", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("health should be public, got %d", rec.Code)
	}
	rec := doRequest(t, handler, http.MethodGet, "/stats", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stats without token = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 response is missing WWW-Authenticate")
	}

	for name, headers := range map[string]map[string]string{
		"bearer":      {"Authorization": "Bearer secret"},
		"x-api-token": {"X-API-Token": "secret"},
	} {
		rec := doRequest(t, handler, http.MethodGet, "/stats", "", headers)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s auth = %d, want 200", name, rec.Code)
		}
	}

	rec = doRequest(t, handler, http.MethodGet, "/stats", "", map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodOptions, "/entries", "", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{})
	rec := doRequest(t, handler, http.MethodGet, "/health", "", map[string]string{"Origin": "https://example.com"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("wildcard CORS = %q, want *", got)
	}

	_, restricted := newHTTPHandler(t, Config{CORSOrigins: []string{"https://allowed.example"}})
	rec = doRequest(t, restricted, http.MethodGet, "/health", "", map[string]string{"Origin": "https://allowed.example"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("allowed origin = %q", got)
	}
	rec = doRequest(t, restricted, http.MethodGet, "/health", "", map[string]string{"Origin": "https://evil.example"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin leaked header %q", got)
	}
	rec = doRequest(t, restricted, http.MethodOptions, "/entries", "", map[string]string{"Origin": "https://allowed.example"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight does not allow Authorization: %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestCheckEndpoint(t *testing.T) {
	store, handler := newHTTPHandler(t, Config{})
	ctx := context.Background()
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "42", ReasonCode: "artist"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "keyword", Value: "秘密", ReasonCode: "content"}); err != nil {
		t.Fatal(err)
	}

	body := `{"items":[{"gid":"42","title":"whatever"},{"gid":"7","title":"这是秘密标题"},{"gid":"8","title":"nothing"}]}`
	rec := doRequest(t, handler, http.MethodPost, "/check", body, map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("check = %d: %s", rec.Code, rec.Body.String())
	}
	var resp CheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(resp.Items))
	}
	if !resp.Items[0].Blacklisted || resp.Items[0].Match == nil || resp.Items[0].Match.Type != "id" {
		t.Fatalf("ID did not match: %+v", resp.Items[0])
	}
	if !resp.Items[1].Blacklisted || resp.Items[1].Match == nil || resp.Items[1].Match.Type != "keyword" {
		t.Fatalf("keyword did not match: %+v", resp.Items[1])
	}
	if resp.Items[2].Blacklisted || resp.Items[2].Match != nil {
		t.Fatalf("unexpected match: %+v", resp.Items[2])
	}

	rec = doRequest(t, handler, http.MethodGet, "/check", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /check = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
	}

	rec = doRequest(t, handler, http.MethodPost, "/check", `{"items":[],"unexpected":1}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown JSON field = %d, want 400", rec.Code)
	}
}

func TestCheckLimit(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{MaxCheckItems: 2})
	body := `{"items":[{"gid":"1"},{"gid":"2"},{"gid":"3"}]}`
	rec := doRequest(t, handler, http.MethodPost, "/check", body, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("check limit = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too many items") {
		t.Fatalf("unexpected error: %s", rec.Body.String())
	}
}

func TestEntriesCRUD(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{})

	rec := doRequest(t, handler, http.MethodPost, "/entries",
		`{"type":"keyword","value":"测试","reason_code":"content"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create entry = %d: %s", rec.Code, rec.Body.String())
	}
	var created Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ValueNorm != "测试" || created.ReasonLabel != "题材避雷" {
		t.Fatalf("unexpected created entry: %+v", created)
	}

	rec = doRequest(t, handler, http.MethodGet, "/entries?type=keyword&limit=10", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list entries = %d", rec.Code)
	}
	var list struct {
		Items []Entry `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("list = %d entries, want 1", len(list.Items))
	}

	target := "/entries/keyword/" + url.PathEscape("测试")
	rec = doRequest(t, handler, http.MethodPut, target, `{"reason_code":"other","priority":7}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("update entry = %d: %s", rec.Code, rec.Body.String())
	}
	var updated Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ReasonLabel != "其他" || updated.Priority != 7 {
		t.Fatalf("update did not persist: %+v", updated)
	}

	rec = doRequest(t, handler, http.MethodDelete, target, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete entry = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, handler, http.MethodDelete, target, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", rec.Code)
	}

	rec = doRequest(t, handler, http.MethodPut, "/entries/keyword/other", `{"type":"keyword","value_norm":"测试"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched path = %d, want 400", rec.Code)
	}
}

func TestReasonsHTTP(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{})

	rec := doRequest(t, handler, http.MethodGet, "/reasons", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list reasons = %d", rec.Code)
	}
	var listed struct {
		Items []Reason `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 8 {
		t.Fatalf("reasons = %d, want 8", len(listed.Items))
	}

	rec = doRequest(t, handler, http.MethodPost, "/reasons", `{"code":"custom","label":"自定义","sort_order":5}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("create reason = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, http.MethodPut, "/reasons/custom", `{"label":"自定义2"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("update reason = %d: %s", rec.Code, rec.Body.String())
	}

	rec = doRequest(t, handler, http.MethodDelete, "/reasons/custom", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete reason = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, handler, http.MethodDelete, "/reasons/custom", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete reason = %d, want 404", rec.Code)
	}
}

func TestExportAndImportHTTP(t *testing.T) {
	store, handler := newHTTPHandler(t, Config{})
	ctx := context.Background()
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "9", ReasonCode: "artist"}); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, handler, http.MethodGet, "/export", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d: %s", rec.Code, rec.Body.String())
	}
	if disposition := rec.Header().Get("Content-Disposition"); !strings.Contains(disposition, "blacklist-export.json") {
		t.Fatalf("Content-Disposition = %q", disposition)
	}
	var exported ExportData
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("export is not valid JSON: %v", err)
	}
	if len(exported.Entries) != 1 {
		t.Fatalf("export entries = %d, want 1", len(exported.Entries))
	}

	rec = doRequest(t, handler, http.MethodPost, "/import?overwrite=false",
		`{"ids":["10"],"keywords":["New Keyword"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d: %s", rec.Code, rec.Body.String())
	}
	var imported ImportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &imported); err != nil {
		t.Fatal(err)
	}
	if imported.ImportedEntries != 2 {
		t.Fatalf("unexpected import result: %+v", imported)
	}

	rec = doRequest(t, handler, http.MethodPost, "/import", `{`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid import = %d, want 400", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{MaxBodyBytes: 64})
	body := `{"type":"keyword","value":"` + strings.Repeat("a", 200) + `"}`
	rec := doRequest(t, handler, http.MethodPost, "/entries", body, map[string]string{"Origin": "https://example.com"})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("413 response lost CORS headers: %q", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	_, handler := newHTTPHandler(t, Config{})
	rec := doRequest(t, handler, http.MethodDelete, "/health", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /health = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Fatalf("Allow = %q, want GET", rec.Header().Get("Allow"))
	}
}

func TestHealthAndStats(t *testing.T) {
	store, handler := newHTTPHandler(t, Config{})
	ctx := context.Background()
	if _, err := store.UpsertEntry(ctx, EntryRequest{Type: "id", Value: "1"}); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, handler, http.MethodGet, "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d", rec.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["ok"] != true {
		t.Fatalf("unexpected health payload: %+v", health)
	}

	rec = doRequest(t, handler, http.MethodGet, "/stats", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d", rec.Code)
	}
	var stats Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.IDs != 1 || stats.Total != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}
