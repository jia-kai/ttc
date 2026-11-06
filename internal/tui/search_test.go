package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestSearchTermsAndMatching(t *testing.T) {
	pattern := newSearchPattern("  DRAFT\tport draft\nİ  ")
	if want := []string{"draft", "port", "i"}; !reflect.DeepEqual(pattern.terms, want) {
		t.Fatalf("got %v, want %v", pattern.terms, want)
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
		if got := pattern.matches(strings.ToLower(tt.text)); got != tt.want {
			t.Fatalf("%q: got %v, want %v", tt.text, got, tt.want)
		}
	}
	if !newSearchPattern(" \t").matches("anything") {
		t.Fatal("empty query must match all text")
	}
}

func TestSearchNonLetterSeparators(t *testing.T) {
	for _, separator := range []string{"-", "_", ".", "/", "\\", ":", "@", "#", "+", "=", "!?", "123", "🙂", "—", "\t", "\u2003"} {
		t.Run(separator, func(t *testing.T) {
			pattern := newSearchPattern("ABC" + separator + "def")
			if !reflect.DeepEqual(pattern.terms, []string{"abc", "def"}) {
				t.Fatal("non-letter did not split terms", pattern.terms)
			}
			if !pattern.matches("abcxyz"+separator+"uvdefww") || !pattern.matches("defww"+separator+"abcxyz") || pattern.matches("abcxyz"+separator) {
				t.Fatal("terms must match substrings in any order")
			}
		})
	}
	if got := newSearchPattern("Été_中文-αβ").terms; !reflect.DeepEqual(got, []string{"中文", "été", "αβ"}) {
		t.Fatal("Unicode letters must remain in terms", got)
	}
}

func TestSearchSymbolSubsequence(t *testing.T) {
	for _, tt := range []struct {
		query, text string
		want        bool
	}{
		{"abc-def", "abcxyz-uvdefww", true},
		{"abc-def", "abcxyz_uvdefww", false},
		{"abc-def", "abcdef", false},
		{"abc-_.def", "abc-x_y.zdef", true},
		{"abc-_.def", "abc._-def", false},
		{"abc--def", "abc-x-def", true},
		{"abc--def", "abc-def", false},
		{"abc12def", "abc1-x-2def", true},
		{"abc12def", "abc21def", false},
		{"abc🙂—def", "abc🙂x—def", true},
		{"abc🙂—def", "abc—🙂def", false},
		{"abcⅣdef", "abcⅣdef", true},
		{"abcⅣdef", "abcⅳdef", true},
		{"abcⅣdef", "abcⅥdef", false},
		{"abcⒶdef", "abcⒶdef", true},
		{"abcⒶdef", "abcⓐdef", true},
		{"--", "a-b-c", true},
		{"--", "a-bc", false},
		{"123_-🙂", "anything", false},
		{"abc def", "defabc", true},
	} {
		if got := newSearchPattern(tt.query).matches(strings.ToLower(tt.text)); got != tt.want {
			t.Errorf("query %q, text %q: got %v, want %v", tt.query, tt.text, got, tt.want)
		}
	}
}
