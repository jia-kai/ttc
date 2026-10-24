package render

import (
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	"reflect"
	"strings"
	"testing"
)

func TestMathChunkCutLongParagraph(t *testing.T) {
	for _, expression := range []string{"$x^2 + y^2$", "$$\nx^2 + y^2\n$$", `\(x^2 + y^2\)`, `\[x^2 + y^2\]`} {
		source := strings.Repeat("a", (16<<10)-4) + expression + " rest"
		cut := MathChunkCut(source, 16<<10)
		if cut != (16<<10)-4 {
			t.Fatalf("%q cut=%d", expression, cut)
		}
		count := 0
		Math(source[cut:], func(string, bool) string { count++; return "M" })
		if count != 1 {
			t.Fatalf("%q formulas=%d tail=%q", expression, count, source[cut:])
		}
	}
}

func plainTerminal(t *testing.T, source string, width int) string {
	t.Helper()
	text, err := Terminal(source, width)
	if err != nil {
		t.Fatal(err)
	}
	return ansi.Strip(text)
}

func TestGridExactListBlocksAndSpacing(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		width              int
	}{
		{"tight", "- one two three four\n- next", "• one two\n  three four\n• next", 12},
		{"loose", "- first\n\n  second\n\n- last", "• first\n  \n  second\n\n• last", 40},
		{"all children", "- first\n\n  second\n\n  > quote\n  >\n  > tail\n\n  ```text\n  a  b\n  ```\n\n  | A | B |\n  |---|---|\n  | x | y |\n\n  - nested",
			"• first\n  \n  second\n  \n  │ quote\n  │ \n  │ tail\n  \n    a  b\n  \n  A │ B\n  ──┼──\n  x │ y\n  \n  • nested", 40},
		{"ordered", "9. one two three\n10. four five six", "9. one two\n   three\n10. four five\n    six", 13},
		{"quote", "> one two three\n>\n> - four five six", "│ one two\n│ three\n│ \n│ • four\n│   five six", 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := plainTerminal(t, test.source, test.width); got != test.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, test.want)
			}
		})
	}
}

func TestGridCodeBlockIndentation(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		width              int
	}{
		{"fenced and wrapped", "```text\nabcdef\n\n  xy\n```", "  abcd\n  ef\n  \n    xy", 6},
		{"indented source", "    code\n      inner", "  code\n    inner", 80},
		{"nested containers", "- item\n\n  > ```text\n  > abcd\n  > ```", "• item\n  \n  │   abcd", 10},
		{"narrow", "```text\n界ab\n```", "界\nab", 2},
		{"partial indent", "```text\n界ab\n```", " 界\n ab", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := plainTerminal(t, test.source, test.width); got != test.want {
				t.Fatalf("got %q; want %q", got, test.want)
			}
		})
	}
}

func TestGridInlineFormulaMarkerBaseline(t *testing.T) {
	text, err := TerminalWithAssets("- before $tall$ after", 80, fixtureMath)
	if err != nil {
		t.Fatal(err)
	}
	want := "         @@@@@@      \n         @@@@@@      \n• before @@@@@@ after"
	if got := ansi.Strip(text); got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestGridDisplayMathNormalizedInsideContainers(t *testing.T) {
	for _, source := range []string{
		"> $$\n> a +\n>\n> b\n> $$",
		"> - $$\n>   a +\n>\n>   b\n>   $$",
		"- \\[\n  a +\n\n  b\n  \\]",
	} {
		var formulas []string
		text, err := TerminalWithAssets(source, 40, func(asset Asset, width int) FixedBox {
			tex, display := asset.Source, asset.Display
			if !display {
				t.Fatal("display formula parsed inline")
			}
			formulas = append(formulas, tex)
			return FixedBox{Rows: []string{"M"}, Width: 1}
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(formulas, []string{"\na +\n\nb\n"}) {
			t.Fatalf("%q: formulas=%q output=%q", source, formulas, ansi.Strip(text))
		}
	}
	if got := plainTerminal(t, "before $$x$$ after", 40); got != "before\nx\nafter" {
		t.Fatalf("display boundaries: %q", got)
	}
}

func TestGridAllTablesShareLayout(t *testing.T) {
	for _, width := range []int{1, 2, 8, 20} {
		plain := plainTerminal(t, "| A | B |\n|---|---:|\n| x | yy |", width)
		math := plainTerminal(t, "| $A$ | B |\n|---|---:|\n| $x$ | yy |", width)
		if plain != math {
			t.Fatalf("width %d: plain=%q math=%q", width, plain, math)
		}
	}
}

func TestGridNestedStylesSurviveFormulaAndWrapping(t *testing.T) {
	text, err := TerminalWithAssets("**A *B $x$ C* D** E", 8, func(Asset, int) FixedBox { return FixedBox{Rows: []string{"M"}, Width: 1} })
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint8{"A": uv.AttrBold, "B": uv.AttrBold | uv.AttrItalic, "C": uv.AttrBold | uv.AttrItalic, "D": uv.AttrBold, "E": 0}
	var style uv.Style
	var parser ansi.Parser
	parser.SetParamsSize(32)
	var state byte
	seen := 0
	for len(text) > 0 {
		seq, _, n, next := ansi.DecodeSequence(text, state, &parser)
		text, state = text[n:], next
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			uv.ReadStyle(parser.Params(), &style)
		} else if attrs, ok := want[seq]; ok {
			if style.Attrs != attrs {
				t.Fatalf("%s attrs=%v; want %v", seq, style.Attrs, attrs)
			}
			seen++
		}
	}
	if seen != len(want) {
		t.Fatalf("seen=%d", seen)
	}
}

func TestGridDecodedEntitiesCannotInjectControls(t *testing.T) {
	text, err := Terminal("&#27;[2J &#x202e; &#xE000; [safe](https://example.test/&#27;[2J)", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "\x1b[2J") || strings.ContainsRune(text, '\u202e') {
		t.Fatalf("unsafe decoded entity: %q", text)
	}
	if !strings.ContainsRune(ansi.Strip(text), '\ue000') {
		t.Fatal("lost printable private-use entity")
	}
}

func TestUnfinishedDisplayDoesNotConsumeMarkdown(t *testing.T) {
	for _, source := range []string{
		"$$\nx\nnot $$\n# Heading\n\n**tail**",
		"> $$\n> x\n>\n> # Heading\n\n$$\n\n**tail**",
	} {
		calls := 0
		text, err := TerminalWithAssets(source, 80, func(Asset, int) FixedBox { calls++; return FixedBox{Rows: []string{"M"}, Width: 1} })
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(text)
		if calls != 0 || !strings.Contains(plain, "Heading") || !strings.Contains(plain, "tail") || strings.Contains(plain, "# Heading") || strings.Contains(plain, "**tail**") {
			t.Fatalf("calls=%d output=%q", calls, plain)
		}
	}
}

func TestOversizedDisplayOpeningLineRemainsLiteral(t *testing.T) {
	for _, prefix := range []string{"", "> ", "- "} {
		source := prefix + "$$" + strings.Repeat(" ", maxFormulaBytes+1) + "\nx\n$$\n\n# Tail"
		calls := 0
		text, err := TerminalWithAssets(source, 80, func(Asset, int) FixedBox {
			calls++
			return FixedBox{Rows: []string{"M"}, Width: 1}
		})
		if err != nil || calls != 0 || !strings.Contains(ansi.Strip(text), "Tail") {
			t.Fatalf("prefix %q: calls=%d err=%v output=%q", prefix, calls, err, text)
		}
	}
}

func TestDisplayLookaheadUsesContainerTabColumns(t *testing.T) {
	for _, source := range []string{
		">  - $$\n>    **INNER**\n> \t> x\n>    $$\n\n# SENTINEL",
		">  - $$\n>    # INNER\n> \t> x\n>    $$\n\n# SENTINEL",
	} {
		document := ParseMarkdown(source)
		_ = ast.Walk(document.root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if formula, ok := node.(*displayFormula); entering && ok && !formula.closed {
				t.Fatalf("committed unfinished display in %q", source)
			}
			return ast.WalkContinue, nil
		})
		out, err := document.Render(80, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		plain := ansi.Strip(out)
		if !strings.Contains(plain, "INNER") || !strings.Contains(plain, "SENTINEL") || strings.Contains(plain, "**INNER**") || strings.Contains(plain, "# INNER") {
			t.Fatalf("lost Markdown structure: %q", plain)
		}
	}
	for _, source := range []string{
		">\t$$\n>\tx\n>\t$$",
		"-\t$$\n\tx\n\t$$",
		"Term\n: $$\n  x\n  $$",
	} {
		calls := 0
		_, err := TerminalWithAssets(source, 80, func(Asset, int) FixedBox { calls++; return FixedBox{Rows: []string{"M"}, Width: 1} })
		if err != nil || calls != 1 {
			t.Fatalf("valid display %q: calls=%d err=%v", source, calls, err)
		}
	}
}

func TestImageMathLabelsRemainLiteral(t *testing.T) {
	for _, label := range []string{`a $\{x\}$ b`, `a \(x\) b`} {
		if got := plainTerminal(t, "!["+label+"](z)", 80); got != "Image: "+label+" → z" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestOrderedListMayStartAtZero(t *testing.T) {
	if got := plainTerminal(t, "0. zero\n1. one", 80); got != "0. zero\n1. one" {
		t.Fatalf("got %q", got)
	}
}

func TestParsedDocumentCanBeReused(t *testing.T) {
	source := "- **before $tall$ after**\n\n  > tail\n\n| A | B |\n|---|---|\n| $x$ | value |"
	document := ParseMarkdown(source)
	for _, width := range []int{40, 8, 40} {
		got, err := document.Render(width, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		want, err := TerminalWithAssets(source, width, fixtureMath)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("width %d: reused document changed output", width)
		}
	}
}
