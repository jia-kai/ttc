package tui

import (
	"slices"
	"strings"
	"unicode"
)

// searchPattern is shared by path completion and prompt history. Letter runs
// match substrings in any order; non-letter, non-space characters (including
// digits) must also occur in order as a subsequence. Whitespace only splits words.
// Candidate text is folded once by the caller with strings.ToLower.
type searchPattern struct {
	terms   []string // Folded, deduplicated AND terms, longest first.
	symbols []rune   // Ordered separator constraints; repetitions are significant.
}

func newSearchPattern(query string) searchPattern {
	var p searchPattern
	query = strings.ToLower(query)
	p.terms = strings.FieldsFunc(query, func(r rune) bool { return !unicode.IsLetter(r) })
	for _, r := range query {
		if !unicode.IsLetter(r) && !unicode.IsSpace(r) {
			p.symbols = append(p.symbols, r)
		}
	}
	slices.SortFunc(p.terms, func(a, b string) int {
		if size := len(b) - len(a); size != 0 {
			return size
		}
		return strings.Compare(a, b)
	})
	p.terms = slices.Compact(p.terms)
	return p
}

func (p searchPattern) matches(folded string) bool {
	for _, term := range p.terms {
		if !strings.Contains(folded, term) {
			return false
		}
	}
	if len(p.symbols) == 0 {
		return true
	}
	next := 0
	for _, r := range folded {
		if r == p.symbols[next] {
			next++
			if next == len(p.symbols) {
				return true
			}
		}
	}
	return false
}
