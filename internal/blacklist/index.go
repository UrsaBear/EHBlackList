package blacklist

import (
	"sort"
	"strings"
)

// index is an immutable snapshot of the whole blacklist. Writes rebuild it,
// which is cheap at this scale and keeps reads lock-free apart from a short
// RWMutex hand-off.
type index struct {
	ids     map[string]Entry
	matcher *matcher
}

func newIndex(entries []Entry) *index {
	ids := make(map[string]Entry, len(entries)/2)
	keywords := make([]Entry, 0, len(entries))
	for _, e := range entries {
		switch e.Type {
		case "id":
			ids[e.ValueNorm] = e
		case "keyword":
			if e.ValueNorm != "" {
				keywords = append(keywords, e)
			}
		}
	}
	sort.SliceStable(keywords, func(i, j int) bool {
		return lessEntry(keywords[i], keywords[j])
	})
	return &index{ids: ids, matcher: newMatcher(keywords)}
}

func (ix *index) match(gid, title string) (Entry, bool) {
	if ix == nil {
		return Entry{}, false
	}
	gid = strings.TrimSpace(gid)
	if gid != "" {
		if e, ok := ix.ids[gid]; ok {
			return e, true
		}
	}
	normalized := normalizeText(title)
	if normalized == "" {
		return Entry{}, false
	}
	return ix.matcher.best(normalized)
}

// lessEntry is the canonical match order: lower priority wins, then older
// entries, then a stable lexical tie-break.
func lessEntry(a, b Entry) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.ValueNorm < b.ValueNorm
}

// matcher is an Aho-Corasick automaton over normalized keyword strings. It
// finds every keyword contained in a title in O(len(title) + matches) instead
// of scanning the keyword list for every title.
type matcher struct {
	entries []Entry
	nodes   []acNode
}

type acNode struct {
	next map[rune]int
	fail int
	out  []int
}

func newMatcher(entries []Entry) *matcher {
	m := &matcher{entries: entries, nodes: []acNode{{}}}
	for i := range entries {
		m.add(entries[i].ValueNorm, i)
	}
	m.build()
	return m
}

func (m *matcher) add(pattern string, idx int) {
	if pattern == "" {
		return
	}
	state := 0
	for _, r := range pattern {
		if m.nodes[state].next == nil {
			m.nodes[state].next = make(map[rune]int)
		}
		next, ok := m.nodes[state].next[r]
		if !ok {
			next = len(m.nodes)
			m.nodes = append(m.nodes, acNode{})
			m.nodes[state].next[r] = next
		}
		state = next
	}
	m.nodes[state].out = append(m.nodes[state].out, idx)
}

func (m *matcher) build() {
	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		queue = append(queue, child)
	}
	for head := 0; head < len(queue); head++ {
		v := queue[head]
		for r, u := range m.nodes[v].next {
			f := m.nodes[v].fail
			for f != 0 {
				if _, ok := m.nodes[f].next[r]; ok {
					break
				}
				f = m.nodes[f].fail
			}
			if next, ok := m.nodes[f].next[r]; ok && next != u {
				m.nodes[u].fail = next
			}
			m.nodes[u].out = append(m.nodes[u].out, m.nodes[m.nodes[u].fail].out...)
			queue = append(queue, u)
		}
	}
}

func (m *matcher) best(text string) (Entry, bool) {
	if m == nil || len(m.nodes) == 0 || text == "" {
		return Entry{}, false
	}
	state := 0
	best := -1
	for _, r := range text {
		for state != 0 {
			if _, ok := m.nodes[state].next[r]; ok {
				break
			}
			state = m.nodes[state].fail
		}
		if next, ok := m.nodes[state].next[r]; ok {
			state = next
		} else {
			state = 0
		}
		for _, idx := range m.nodes[state].out {
			if best < 0 || lessEntry(m.entries[idx], m.entries[best]) {
				best = idx
			}
		}
	}
	if best < 0 {
		return Entry{}, false
	}
	return m.entries[best], true
}
