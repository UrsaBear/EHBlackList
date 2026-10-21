package blacklist

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestIndexPriorityAndIDShortCircuit(t *testing.T) {
	ix := newIndex([]Entry{
		{Type: "keyword", Value: "alpha", ValueNorm: "alpha", Priority: 20, CreatedAt: 100},
		{Type: "keyword", Value: "alpha beta", ValueNorm: "alphabeta", Priority: 10, CreatedAt: 100},
		{Type: "id", Value: "123", ValueNorm: "123", Priority: 999, CreatedAt: 100},
	})

	entry, ok := ix.match("", "alpha beta gamma")
	if !ok || entry.ValueNorm != "alphabeta" {
		t.Fatalf("expected highest-priority keyword alphabeta, got %+v (ok=%v)", entry, ok)
	}

	entry, ok = ix.match("123", "alpha beta")
	if !ok || entry.Type != "id" {
		t.Fatalf("expected ID to win over keyword, got %+v (ok=%v)", entry, ok)
	}
}

func TestIndexTieBreak(t *testing.T) {
	ix := newIndex([]Entry{
		{Type: "keyword", Value: "ab", ValueNorm: "ab", Priority: 10, CreatedAt: 50},
		{Type: "keyword", Value: "abc", ValueNorm: "abc", Priority: 10, CreatedAt: 100},
	})
	entry, ok := ix.match("", "abc")
	if !ok || entry.ValueNorm != "ab" {
		t.Fatalf("expected older keyword ab to win the tie, got %+v", entry)
	}
}

func TestIndexEmpty(t *testing.T) {
	ix := newIndex(nil)
	if _, ok := ix.match("", "anything"); ok {
		t.Fatal("empty index should not match")
	}
	if _, ok := ix.match("", ""); ok {
		t.Fatal("empty title should not match")
	}
}

func TestMatcherMatchesNaiveScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []rune("abcde")

	for trial := 0; trial < 500; trial++ {
		entries := make([]Entry, 0, 8)
		seen := make(map[string]bool)
		for i := 0; i < 8; i++ {
			pattern := randomString(rng, alphabet, 4)
			if pattern == "" || seen[pattern] {
				continue
			}
			seen[pattern] = true
			entries = append(entries, Entry{
				Type:      "keyword",
				Value:     pattern,
				ValueNorm: pattern,
				Priority:  rng.Intn(4),
				CreatedAt: int64(rng.Intn(3)),
			})
		}

		ix := newIndex(entries)
		title := randomString(rng, alphabet, 30)
		got, gotOK := ix.match("", title)

		normalized := normalizeText(title)
		sorted := append([]Entry(nil), entries...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return lessEntry(sorted[i], sorted[j])
		})
		var want Entry
		wantOK := false
		for _, e := range sorted {
			if e.ValueNorm != "" && strings.Contains(normalized, e.ValueNorm) {
				want = e
				wantOK = true
				break
			}
		}

		if gotOK != wantOK {
			t.Fatalf("trial %d: matcher ok=%v, naive ok=%v (title=%q)", trial, gotOK, wantOK, title)
		}
		if gotOK && got.ValueNorm != want.ValueNorm {
			t.Fatalf("trial %d: matcher=%q, naive=%q (title=%q)", trial, got.ValueNorm, want.ValueNorm, title)
		}
	}
}

func randomString(rng *rand.Rand, alphabet []rune, maxLen int) string {
	n := rng.Intn(maxLen + 1)
	out := make([]rune, n)
	for i := range out {
		out[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(out)
}
