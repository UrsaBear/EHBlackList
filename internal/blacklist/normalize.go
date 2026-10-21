package blacklist

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsValidationError reports whether err was caused by invalid client input.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func invalidf(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

func normalizeEntryRequest(req EntryRequest) (Entry, error) {
	typ := strings.TrimSpace(req.Type)
	if typ != "id" && typ != "keyword" {
		return Entry{}, invalidf("invalid type: %s", typ)
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

	priority := DefaultPriority
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
		return Reason{}, invalidf("reason label is required")
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
	switch typ {
	case "id":
		if value == "" {
			return "", invalidf("id value is required")
		}
		for _, r := range value {
			if r < '0' || r > '9' {
				return "", invalidf("invalid id: %s", value)
			}
		}
		return value, nil
	case "keyword":
		norm := normalizeText(value)
		if norm == "" {
			return "", invalidf("keyword is empty after normalization")
		}
		return norm, nil
	default:
		return "", invalidf("invalid type: %s", typ)
	}
}

func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// normalizeText lowercases text and removes whitespace and punctuation. It is
// shared by keyword storage and title matching, so both sides always agree.
// All full-width punctuation is removed just like its ASCII counterpart, which
// keeps the hot path allocation-free (no per-call punctuation map).
func normalizeText(str string) string {
	if str == "" {
		return ""
	}
	s := strings.ToLower(str)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
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
	switch r {
	case '.', ',', '!', '?', '-', '_', ':', ';', '\'', '"', '`', '~',
		'[', ']', '(', ')', '{', '}', '<', '>', '/', '\\':
		return true
	case '，', '。', '！', '？', '：', '；', '…', '【', '】', '「', '」', '『', '』',
		'、', '《', '》', '（', '）', '—', '－', '～', '·':
		return true
	}
	return false
}

func trimReason(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 40 {
		return string(runes[:40])
	}
	return s
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
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
