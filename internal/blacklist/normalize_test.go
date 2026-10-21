package blacklist

import (
	"strings"
	"testing"
)

func TestNormalizeText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Hello, World!", "helloworld"},
		{"测试，关键词！", "测试关键词"},
		{"ＡＢＣ　１２３", "ａｂｃ１２３"},
		{"foo_bar-baz", "foobarbaz"},
		{"「測試」《關鍵詞》", "測試關鍵詞"},
		{"  A  B  ", "ab"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeText(tc.in); got != tc.want {
			t.Errorf("normalizeText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeValue(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		value   string
		want    string
		wantErr bool
	}{
		{"id", "id", "123456", "123456", false},
		{"id trims spaces", "id", " 123 ", "123", false},
		{"id keeps leading zero", "id", "00123", "00123", false},
		{"id empty", "id", "", "", true},
		{"id non digit", "id", "12a", "", true},
		{"keyword", "keyword", "Foo, Bar!", "foobar", false},
		{"keyword punctuation only", "keyword", "!!!", "", true},
		{"unknown type", "bogus", "x", "", true},
	}
	for _, tc := range cases {
		got, err := normalizeValue(tc.typ, tc.value)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeEntryRequest(t *testing.T) {
	priority := 7
	entry, err := normalizeEntryRequest(EntryRequest{
		Type:     "keyword",
		Value:    "  Test  ",
		Priority: &priority,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Value != "Test" || entry.ValueNorm != "test" || entry.Priority != 7 {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	entry, err = normalizeEntryRequest(EntryRequest{Type: "id", ValueNorm: "123"})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Value != "123" || entry.Priority != DefaultPriority {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	if _, err := normalizeEntryRequest(EntryRequest{Type: "bogus", Value: "x"}); !IsValidationError(err) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		" Hello, World! ": "hello-world",
		"abc__def":        "abc-def",
		"---":             "",
		"":                "",
		"A1_b2":           "a1-b2",
	}
	for in, want := range cases {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimReason(t *testing.T) {
	long := strings.Repeat("字", 50)
	if got := trimReason(long); len([]rune(got)) != 40 {
		t.Fatalf("trimReason length = %d, want 40", len([]rune(got)))
	}
	if got := trimReason("  不喜欢作者  "); got != "不喜欢作者" {
		t.Fatalf("trimReason = %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("你好世界", 2); got != "你好" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("truncateRunes = %q", got)
	}
}
