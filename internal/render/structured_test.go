package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestStructuredJSONTokensAndPartialStrings(t *testing.T) {
	for _, source := range []string{
		` ["escaped \"quote", -1.25e+3, true, false, null, {}, []]`,
		` "unfinished escaped \"quote and trailing \`,
		` "` + strings.Repeat("界", 8000),
		` prose42 trueish +12 01 1e unknown`,
	} {
		t.Run(source[:min(20, len(source))], func(t *testing.T) {
			colored := structuredJSON(source)
			if got := ansi.Strip(colored); got != source {
				t.Fatalf("highlighting changed source: got %q, want %q", got, source)
			}
		})
	}
	for _, token := range []struct{ text, color string }{
		{`"escaped \"quote"`, AmberColor},
		{"-1.25e+3", CyanColor},
		{"true", LavenderColor},
		{"false", LavenderColor},
		{"null", LavenderColor},
		{"[", MutedColor},
	} {
		if got := structuredJSON(token.text); got != inspectionColor(token.text, token.color) {
			t.Errorf("token %q: got %q", token.text, got)
		}
	}
	if got := structuredJSON(" prose42 trueish +12 01 1e unknown"); strings.Contains(got, "\x1b") {
		t.Fatalf("non-JSON scalars highlighted: %q", got)
	}
	partial := `"unfinished \" quote and trailing \`
	if got := structuredJSON(partial); got != inspectionColor(partial, AmberColor) {
		t.Fatalf("partial string is not one styled span: %q", got)
	}
}

func TestStructuredPartialStringAllocationsDoNotScaleWithCharacters(t *testing.T) {
	short := `  text: "` + strings.Repeat("a", 64)
	long := `  text: "` + strings.Repeat("a", 16384)
	allocations := func(source string) float64 {
		return testing.AllocsPerRun(10, func() {
			StructuredPage(source, StructuredState{}, true)
		})
	}
	// Buffer sizes grow with the source, but token/lexer objects must not be
	// allocated per character when a bounded page ends inside a string.
	if small, large := allocations(short), allocations(long); large > small+10 {
		t.Fatalf("partial string allocations scale with characters: short %.0f, long %.0f", small, large)
	}
}
