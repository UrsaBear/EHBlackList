// Package blacklist implements the EH blacklist API server.
//
// The service is a small, single-binary JSON API backed by SQLite. It is
// intentionally stateless apart from the database and keeps the whole
// blacklist in memory so that batch checks never touch SQLite.
package blacklist

const (
	// DefaultAddr is the default listen address.
	DefaultAddr = ":8787"
	// DefaultDBPath is the default SQLite file, relative to the working
	// directory.
	DefaultDBPath = "./data/blacklist.db"

	// DefaultLimit is the page size used by GET /entries when no limit is
	// supplied.
	DefaultLimit = 200
	// MaxLimit is the largest page size accepted by GET /entries.
	MaxLimit = 5000

	// DefaultPriority is used when an entry does not specify one. Lower
	// numbers sort first.
	DefaultPriority = 100

	defaultMaxCheckItems = 1000
	defaultMaxBodyBytes  = 64 << 20

	// maxTitleRunes bounds the work done per item. Gallery titles are much
	// shorter than this; the cap only exists to keep malformed input cheap.
	maxTitleRunes = 4096
	maxGIDRunes   = 128
)

// Entry is one blacklist rule. The pair (Type, ValueNorm) is unique.
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

// EntryRequest is the payload accepted by POST /entries and PUT /entries/....
type EntryRequest struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueNorm   string `json:"value_norm,omitempty"`
	ReasonCode  string `json:"reason_code,omitempty"`
	ReasonLabel string `json:"reason_label,omitempty"`
	Priority    *int   `json:"priority,omitempty"`
}

// Reason is a reusable blacklist reason ("不喜欢作者", ...).
type Reason struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	SortOrder int    `json:"sort_order"`
	Builtin   bool   `json:"builtin"`
}

// ReasonRequest is the payload accepted by POST /reasons and PUT /reasons/....
type ReasonRequest struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	SortOrder *int   `json:"sort_order,omitempty"`
	Builtin   *bool  `json:"builtin,omitempty"`
}

// CheckRequest is the payload accepted by POST /check.
type CheckRequest struct {
	Items []CheckItem `json:"items"`
}

// CheckItem is one gallery to test.
type CheckItem struct {
	GID   string `json:"gid"`
	Title string `json:"title"`
}

// CheckResponse is the result of POST /check.
type CheckResponse struct {
	Items []CheckResult `json:"items"`
}

// CheckResult reports whether a single item matched, and if so, why.
type CheckResult struct {
	GID         string       `json:"gid"`
	Blacklisted bool         `json:"blacklisted"`
	Match       *MatchResult `json:"match"`
}

// MatchResult is the rule that matched an item.
type MatchResult struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ValueNorm   string `json:"value_norm"`
	ReasonCode  string `json:"reason_code,omitempty"`
	ReasonLabel string `json:"reason_label,omitempty"`
	Priority    int    `json:"priority"`
}

// ExportData is the version 2 export/import format.
type ExportData struct {
	Version  int      `json:"version"`
	Reasons  []Reason `json:"reasons"`
	Entries  []Entry  `json:"entries"`
	Exported int64    `json:"exported_at"`
}

// LegacyImport is the old userscript format: {ids: [], keywords: []}.
type LegacyImport struct {
	IDs      []string `json:"ids"`
	Keywords []string `json:"keywords"`
}

// ImportResult summarises an import.
type ImportResult struct {
	ImportedEntries int  `json:"imported_entries"`
	ImportedReasons int  `json:"imported_reasons"`
	Skipped         int  `json:"skipped"`
	Overwrite       bool `json:"overwrite"`
}

// Stats is returned by GET /stats.
type Stats struct {
	IDs      int `json:"ids"`
	Keywords int `json:"keywords"`
	Reasons  int `json:"reasons"`
	Total    int `json:"total"`
}

// APIError is the JSON error envelope.
type APIError struct {
	Error string `json:"error"`
}

// ValidationError marks an error caused by the client rather than the
// database. Handlers translate it to HTTP 400.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

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
