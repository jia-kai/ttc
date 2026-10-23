package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func fixtureMath(tex string, display bool, width int) FixedBox {
	height := 1
	if strings.Contains(tex, "tall") {
		height = 3
	}
	columns := min(6, max(1, width))
	rows := make([]string, height)
	for i := range rows {
		rows[i] = strings.Repeat("@", columns)
	}
	return FixedBox{Rows: rows, Width: columns, Baseline: height - 1}
}

func TestMathTableCarrierPreservesStructureAndOrdering(t *testing.T) {
	source := "Before table.\n\n| **Formula** $tall$ | Meaning |\n| :--- | ---: |\n| $tall$ then $x$ | **bold** and `code` |\n| $$x$$ | [notes](notes.md) 界 |\n| following | intact |\n\nAfter table."
	for _, width := range []int{2, 8, 24, 80} {
		text, err := TerminalMath(source, width, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(text)
		if strings.Index(plain, "Before") > strings.Index(plain, "@") && width >= 24 {
			t.Fatal("table escaped document buffer", plain)
		}
		for _, r := range plain {
			if r >= 0xe000 && r <= 0xf8ff {
				t.Fatalf("leaked placeholder %U: %s", r, plain)
			}
		}
		for _, row := range strings.Split(text, "\n") {
			if ansi.StringWidth(row) > width {
				t.Fatalf("width %d exceeded by %q", width, row)
			}
		}
		if width >= 80 {
			for _, want := range []string{"Formula", "Meaning", "bold", "code", "notes", "界", "following", "intact", "After table."} {
				if !strings.Contains(plain, want) {
					t.Fatalf("lost %q: %s", want, plain)
				}
			}
			if strings.Contains(plain, "| following |") {
				t.Fatal("math terminated table", plain)
			}
		}
	}
}

func TestMathBoxesInContainersAndProse(t *testing.T) {
	for _, source := range []string{
		"before **bold $tall$ after** end",
		"> before $tall$ after\n> next line",
		"- before $tall$ after\n- next item",
		"1. before $tall$ after\n2. next item",
		"# Heading $tall$ after",
		"> | Formula | Text |\n> | --- | --- |\n> | $tall$ | value |\n> | next | intact |",
		"- Table:\n\n  | Formula | Text |\n  | --- | --- |\n  | $tall$ | value |",
	} {
		t.Run(source, func(t *testing.T) {
			for _, width := range []int{8, 24, 80} {
				text, err := TerminalMath(source, width, fixtureMath)
				if err != nil {
					t.Fatal(err)
				}
				plain := ansi.Strip(text)
				if !strings.Contains(plain, "@") {
					t.Fatal("lost math", plain)
				}
				for _, row := range strings.Split(text, "\n") {
					if ansi.StringWidth(row) > width {
						t.Fatalf("width %d exceeded: %q", width, row)
					}
				}
				t.Logf("width %d:\n%s", width, plain)
			}
		})
	}
}

func TestLiteralMathTableKeepsTeXAndCode(t *testing.T) {
	source := "| Formula | Description |\n| --- | --- |\n| $$\\frac{a}{b}$$ | `literal $code$` |\n| $x \\mid y$ | next |"
	text, err := Terminal(source, 80)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(text)
	for _, want := range []string{`\frac{a}{b}`, "literal $code$", `x \mid y`, "next"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q: %s", want, plain)
		}
	}
}

func TestMathCarrierRetainsInlineAndContainerSemantics(t *testing.T) {
	for _, test := range []struct {
		source     string
		want       string
		decoration string
	}{
		{"before **bold $tall$ after** end", "before bold @@@@@@ after end", ""},
		{"- before $tall$ after", "before @@@@@@ after", "•"},
		{"1. before $tall$ after", "before @@@@@@ after", "1."},
		{"- [x] before $tall$ after", "before @@@@@@ after", "[✓]"},
	} {
		text, err := TerminalMath(test.source, 80, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(text)
		if !strings.Contains(plain, test.want) {
			t.Fatalf("inline content unexpectedly split: %s", plain)
		}
		if test.decoration != "" && strings.Count(plain, test.decoration) != 1 {
			t.Fatalf("lost or repeated list marker %q: %s", test.decoration, plain)
		}
	}
	text, err := TerminalMath("before\n$tall$\nafter", 80, fixtureMath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ansi.Strip(text), "before\n@@@@@@\n@@@@@@\n@@@@@@\nafter") {
		t.Fatalf("lost preserved line break: %s", ansi.Strip(text))
	}
}

func TestMathTokensDoNotEscapeThroughLinks(t *testing.T) {
	for _, source := range []string{
		"[notes](https://example.test/$path$)",
		"[value $x$](https://example.test/$path$)",
		"<https://example.test/$path$>",
		"<https://example.test/$path$> and [notes](https://example.test/$path$) $x$",
		"Term $x$\n: Definition",
	} {
		text, err := TerminalMath(source, 80, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(text)
		for _, r := range plain {
			if r >= 0xe000 && r <= 0xf8ff {
				t.Fatalf("token in link output: %s", plain)
			}
		}
		if strings.Contains(source, "example") && strings.Count(plain, "$path$") < strings.Count(source, "$path$") {
			t.Fatalf("URL was treated as math: %s", plain)
		}
	}
}

func TestMathCarriersPreserveLooseListParagraphs(t *testing.T) {
	for _, source := range []string{
		"- first $x$\n\n  second $tall$ after\n\n  last tail",
		"- first plain\n\n  second $tall$ after\n\n  last tail",
		"- [x] first $x$\n\n  second $tall$ after\n\n  last tail",
		"> - first $x$\n>\n>   second $tall$ after\n>\n>   last tail",
	} {
		text, err := TerminalMath(source, 80, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(text)
		for _, want := range []string{"first", "second", "after", "last tail"} {
			if strings.Count(plain, want) != 1 {
				t.Fatalf("lost or repeated %q in %q:\n%s", want, source, plain)
			}
		}
		if strings.Count(plain, "@@@@@@") < 3 {
			t.Fatal("lost tall formula", plain)
		}
	}
	text, err := TerminalMath("Term\n: definition $tall$ after", 80, fixtureMath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(ansi.Strip(text), "🠶") != 1 {
		t.Fatal("repeated definition marker", ansi.Strip(text))
	}
}

func TestMathMarkersReserveDecodedEntities(t *testing.T) {
	source := "&#xE000; before $x$ after\n\n&#xE001; LOST TAIL"
	text, err := TerminalMath(source, 80, fixtureMath)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(text)
	if strings.Count(plain, "@@@@@@") != 1 || !strings.Contains(plain, "LOST TAIL") || !strings.ContainsRune(plain, 0xe000) || !strings.ContainsRune(plain, 0xe001) {
		t.Fatalf("numeric entity collided with internal token:\n%s", plain)
	}
}
