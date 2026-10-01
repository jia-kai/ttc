package render

import (
	"github.com/charmbracelet/x/ansi"
	"os"
	"strings"
	"testing"
)

func TestResearchMarkdownFormatsAndNarrowWidth(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/research/markdown.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, width := range []int{2, 8, 24, 100} {
		text, err := Terminal(string(data)+"\n\x1b[2JCONTROL", width)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, "\x1b[2J") {
			t.Fatal("source control survived")
		}
		for _, row := range strings.Split(text, "\n") {
			if ansi.StringWidth(row) > width {
				t.Fatalf("width %d exceeded: %q", width, row)
			}
		}
		if width == 100 {
			plain := ansi.Strip(text)
			for _, want := range []string{"Research", "4.0", "average", `\frac`} {
				if !strings.Contains(plain, want) {
					t.Fatalf("missing %q: %s", want, plain)
				}
			}
		}
	}
}

func TestJSONFormattingPreservesPrecisionAndWrapsValues(t *testing.T) {
	raw := `{"z":9007199254740993,"a":{"value":"a long sentence with words to wrap and αβ characters","escaped":"literal\\n"}}`
	fence := Fence(raw, "json")
	if !strings.Contains(fence, "9007199254740993") || strings.Index(fence, "\"z\"") > strings.Index(fence, "\"a\"") || !strings.Contains(fence, "literal\\\\n") {
		t.Fatal(fence)
	}
	for _, width := range []int{8, 24, 60} {
		text, err := Terminal(fence, width)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(text, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatal(width, line)
			}
		}
		if !strings.Contains(ansi.Strip(text), "αβ") {
			t.Fatal("lost Unicode", text)
		}
	}
}
