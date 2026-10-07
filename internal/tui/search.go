package tui

import (
	"slices"
	"strings"
)

// searchTerms folds whitespace-separated AND terms, deduplicating and placing
// longer terms first so substring searches can reject nonmatches early.
func searchTerms(query string) []string {
	terms := strings.Fields(strings.ToLower(query))
	slices.SortFunc(terms, func(a, b string) int {
		if size := len(b) - len(a); size != 0 {
			return size
		}
		return strings.Compare(a, b)
	})
	return slices.Compact(terms)
}

// matchesSearchTerms expects text and terms already folded with strings.ToLower.
// Every term must occur as a substring, in any order; no terms matches all text.
func matchesSearchTerms(folded string, terms []string) bool {
	for _, term := range terms {
		if !strings.Contains(folded, term) {
			return false
		}
	}
	return true
}
