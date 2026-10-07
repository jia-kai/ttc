package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestSearchTermsAndMatching(t *testing.T) {
	terms := searchTerms("  DRAFT\tport draft\nİ  ")
	if want := []string{"draft", "port", "i"}; !reflect.DeepEqual(terms, want) {
		t.Fatalf("got %v, want %v", terms, want)
	}
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"Report-DRAFT.İ", true},
		{"İ draft report", true},
		{"Report-DRAFT", false},
		{"DRAFT.İ", false},
	} {
		if got := matchesSearchTerms(strings.ToLower(tt.text), terms); got != tt.want {
			t.Fatalf("%q: got %v, want %v", tt.text, got, tt.want)
		}
	}
	if !matchesSearchTerms("anything", searchTerms(" \t")) {
		t.Fatal("empty query must match all text")
	}
}
