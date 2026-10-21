package blacklist

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Server wires the store and configuration into an http.Handler.
type Server struct {
	store *Store
	cfg   Config
}

// NewServer creates a Server. Zero-valued limits are replaced with defaults so
// that tests (and embedders) cannot accidentally create a server that rejects
// every request.
func NewServer(store *Store, cfg Config) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxCheckItems <= 0 {
		cfg.MaxCheckItems = defaultMaxCheckItems
	}
	return &Server{store: store, cfg: cfg}
}

// Handler returns the fully wrapped HTTP handler. Middleware order:
// logging -> panic recovery -> CORS -> auth -> body limit -> routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	var h http.Handler = mux
	h = s.limitBody(h)
	h = s.authenticate(h)
	h = s.cors(h)
	h = s.recover(h)
	h = s.logging(h)
	return h
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/check", s.handleCheck)
	mux.HandleFunc("/entries", s.handleEntries)
	mux.HandleFunc("/entries/", s.handleEntryByPath)
	mux.HandleFunc("/reasons", s.handleReasons)
	mux.HandleFunc("/reasons/", s.handleReasonByPath)
	mux.HandleFunc("/export", s.handleExport)
	mux.HandleFunc("/import", s.handleImport)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().Unix()})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w, http.MethodPost)
		return
	}
	var req CheckRequest
	if err := readJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if len(req.Items) > s.cfg.MaxCheckItems {
		writeErrorMessage(w, http.StatusBadRequest,
			fmt.Sprintf("too many items: %d (max %d)", len(req.Items), s.cfg.MaxCheckItems))
		return
	}
	writeJSON(w, http.StatusOK, CheckResponse{Items: s.store.Check(req.Items)})
}

func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listEntries(w, r)
	case http.MethodPost, http.MethodPut:
		s.upsertEntryFromBody(w, r)
	case http.MethodDelete:
		s.deleteEntryFromRequest(w, r)
	default:
		writeMethodNotAllowed(w, "GET, POST, PUT, DELETE")
	}
}

func (s *Server) handleEntryByPath(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/entries/"))
	if len(parts) != 2 {
		writeErrorMessage(w, http.StatusBadRequest, "expected /entries/{type}/{value_norm}")
		return
	}
	typ := parts[0]
	valueNorm, err := url.PathUnescape(parts[1])
	if err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "invalid value_norm")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteEntry(r.Context(), typ, valueNorm); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	case http.MethodPut:
		var req EntryRequest
		if err := readJSON(r, &req); err != nil {
			writeDecodeError(w, err)
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
		stored, err := s.store.UpsertEntryNormalized(r.Context(), entry)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, stored)

	default:
		writeMethodNotAllowed(w, "DELETE, PUT")
	}
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := parseIntDefault(r.URL.Query().Get("limit"), DefaultLimit)
	offset := parseIntDefault(r.URL.Query().Get("offset"), 0)
	if limit <= 0 || limit > MaxLimit {
		limit = DefaultLimit
	}
	if offset < 0 {
		offset = 0
	}

	entries, err := s.store.ListEntries(r.Context(), typ, q, limit, offset)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  entries,
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) upsertEntryFromBody(w http.ResponseWriter, r *http.Request) {
	var req EntryRequest
	if err := readJSON(r, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	entry, err := s.store.UpsertEntry(r.Context(), req)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) deleteEntryFromRequest(w http.ResponseWriter, r *http.Request) {
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
	if err := s.store.DeleteEntry(r.Context(), typ, valueNorm); err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleReasons(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		reasons, err := s.store.ListReasons(r.Context())
		if err != nil {
			s.writeInternalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": reasons})

	case http.MethodPost:
		var req ReasonRequest
		if err := readJSON(r, &req); err != nil {
			writeDecodeError(w, err)
			return
		}
		reason, err := s.store.UpsertReason(r.Context(), req)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, reason)

	default:
		writeMethodNotAllowed(w, "GET, POST")
	}
}

func (s *Server) handleReasonByPath(w http.ResponseWriter, r *http.Request) {
	code, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/reasons/"))
	if err != nil {
		writeErrorMessage(w, http.StatusBadRequest, "invalid reason code")
		return
	}
	code = normalizeCode(code)
	if code == "" {
		writeErrorMessage(w, http.StatusBadRequest, "invalid reason code")
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req ReasonRequest
		if err := readJSON(r, &req); err != nil {
			writeDecodeError(w, err)
			return
		}
		req.Code = code
		reason, err := s.store.UpsertReason(r.Context(), req)
		if err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, reason)

	case http.MethodDelete:
		if err := s.store.DeleteReason(r.Context(), code); err != nil {
			s.writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})

	default:
		writeMethodNotAllowed(w, "DELETE, PUT")
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w, http.MethodGet)
		return
	}
	data, err := s.store.Export(r.Context())
	if err != nil {
		s.writeInternalError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="blacklist-export.json"`)
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		writeMethodNotAllowed(w, "POST, PUT")
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	overwrite := parseBoolDefault(r.URL.Query().Get("overwrite"), true)
	res, err := s.store.Import(r.Context(), body, overwrite)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- middleware -----------------------------------------------------------

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.cfg.APIToken == "" {
		return next
	}
	expected := sha256.Sum256([]byte(s.cfg.APIToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if token == "" {
			token = strings.TrimSpace(r.Header.Get("X-API-Token"))
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], expected[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="eh-blacklist"`)
			writeErrorMessage(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowAll := len(s.cfg.CORSOrigins) == 0
	for _, origin := range s.cfg.CORSOrigins {
		if origin == "*" {
			allowAll = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		switch {
		case allowAll:
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case origin != "" && originAllowed(origin, s.cfg.CORSOrigins):
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Token")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > s.cfg.MaxBodyBytes {
			writeErrorMessage(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w}
		defer func() {
			status := lw.status
			if status == 0 {
				status = http.StatusOK
			}
			log.Printf(
				"http_request method=%s path=%q query=%q status=%d bytes=%d duration=%s remote=%q user_agent=%q",
				r.Method, r.URL.Path, r.URL.RawQuery, status, lw.bytes,
				time.Since(start).Round(time.Microsecond), s.clientIP(r), r.UserAgent(),
			)
		}()
		next.ServeHTTP(lw, r)
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic method=%s path=%q err=%v stack=%s",
					r.Method, r.URL.Path, rec, debug.Stack())
				if lw, ok := w.(*loggingResponseWriter); ok && lw.status != 0 {
					return
				}
				writeErrorMessage(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case IsValidationError(err):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrNotFound):
		writeErrorMessage(w, http.StatusNotFound, err.Error())
	default:
		s.writeInternalError(w, err)
	}
}

func (s *Server) writeInternalError(w http.ResponseWriter, err error) {
	log.Printf("internal_error: %v", err)
	writeErrorMessage(w, http.StatusInternalServerError, "internal server error")
}

// --- helpers --------------------------------------------------------------

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

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, APIError{Error: err.Error()})
}

func writeErrorMessage(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, APIError{Error: msg})
}

func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeErrorMessage(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeDecodeError(w http.ResponseWriter, err error) {
	if isBodyTooLarge(err) {
		writeErrorMessage(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

func originAllowed(origin string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate == "*" || strings.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
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
