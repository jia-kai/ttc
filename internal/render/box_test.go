package render

import (
	"reflect"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func plainBoxRows(rows []string) []string {
	result := make([]string, len(rows))
	for i, row := range rows {
		result[i] = ansi.Strip(row)
	}
	return result
}

func boxItem(rows []string, width, baseline int) flowItem {
	return flowItem{box: func(int) FixedBox {
		return FixedBox{Rows: rows, Width: width, Baseline: baseline}
	}}
}

func TestLayoutFlowAtomicBaseline(t *testing.T) {
	items := []flowItem{
		{text: "a "}, boxItem([]string{"XX", "YY", "ZZ"}, 2, 1),
		{text: " b "}, boxItem([]string{"T", "U"}, 1, 0),
	}
	want := []string{"  XX    ", "a YY b T", "  ZZ   U"}
	if got := layoutFlow(items, 20); !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline layout = %q; want %q", got, want)
	}
	want = []string{"a", "XX", "YY", "ZZ", "b T", "  U"}
	if got := layoutFlow(items, 3); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapped rectangles = %q; want %q", got, want)
	}
}

func TestLayoutFlowWordsBlocksAndCallbacks(t *testing.T) {
	var available []int
	block := flowItem{block: true, box: func(width int) FixedBox {
		available = append(available, width)
		return FixedBox{Rows: []string{"12345", "abcde"}, Width: 5, Baseline: 1}
	}}
	items := []flowItem{{text: "one "}, {text: "two three"}, block, {text: "after\nnext"}}
	want := []string{"one two", "three", "12345", "abcde", "after", "next"}
	if got := layoutFlow(items, 7); !reflect.DeepEqual(got, want) {
		t.Fatalf("layout = %q; want %q", got, want)
	}
	if !reflect.DeepEqual(available, []int{7}) {
		t.Fatalf("factory received %v, want full line width", available)
	}
	if got := layoutFlow([]flowItem{boxItem([]string{"12345", "abcde"}, 5, 0)}, 1); !reflect.DeepEqual(got, []string{"12345", "abcde"}) {
		t.Fatalf("oversized rectangle was altered: %q", got)
	}
}

func TestLayoutFlowUnicodeAndANSI(t *testing.T) {
	text := "\x1b[31me\u0301👩‍🔬界abcdef\x1b[0m"
	rows := layoutFlow([]flowItem{{text: text}}, 3)
	if got := strings.Join(plainBoxRows(rows), ""); got != ansi.Strip(text) {
		t.Fatalf("Unicode text changed: %q", got)
	}
	for _, row := range rows {
		if ansi.StringWidth(row) > 3 {
			t.Fatalf("row wider than viewport: %q", row)
		}
	}
	// A two-cell grapheme cannot fit a one-cell viewport but is never split.
	got := plainBoxRows(layoutFlow([]flowItem{{text: "界e\u0301"}}, 1))
	if !reflect.DeepEqual(got, []string{"界", "e\u0301"}) {
		t.Fatalf("grapheme split: %q", got)
	}
	if got := layoutFlow([]flowItem{{text: "\u0301"}}, 1); !reflect.DeepEqual(got, []string{"\u0301"}) {
		t.Fatalf("zero-width grapheme lost: %q", got)
	}
	styled := layoutFlow([]flowItem{{text: "\x1b[31mred green abcdefghijk\x1b[0m"}}, 5)
	for _, row := range styled {
		if !strings.HasPrefix(row, "\x1b[31m") || !strings.HasSuffix(row, ansi.ResetStyle) {
			t.Fatalf("style not isolated/replayed: %q", row)
		}
	}
}

func TestLayoutTableHeightAlignmentAndNarrow(t *testing.T) {
	cells := [][][]flowItem{
		{{{text: "long\nhead"}}, {{text: "B"}}},
		{{boxItem([]string{"xx", "yy", "zz"}, 2, 1)}, {{text: "q"}}},
	}
	got := layoutTable(cells, []int{-1, 1}, 12)
	want := []string{"long │ B", "head │  ", "─────┼──", "xx   │ q", "yy   │  ", "zz   │  "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("table = %q; want %q", got, want)
	}
	narrow := plainBoxRows(layoutTable(cells, nil, 2))
	want = []string{"lo", "ng", "he", "ad", "B", "", "xx", "yy", "zz", "q"}
	if !reflect.DeepEqual(narrow, want) {
		t.Fatalf("stacked table = %q; want %q", narrow, want)
	}
}

func TestLayoutTableFitsAndKeepsAllContent(t *testing.T) {
	cells := [][][]flowItem{
		{{{text: "Name"}}, {{text: "Value"}}, {{text: "界"}}},
		{{{text: "alpha beta"}}, {boxItem([]string{"ABC", "DEF"}, 3, 0)}, {{text: "e\u0301👩‍🔬"}}},
	}
	for width := 1; width <= 30; width++ {
		rows := layoutTable(cells, []int{-1, 0, 1}, width)
		var all strings.Builder
		for _, row := range rows {
			if ansi.StringWidth(row) > max(width, 3) {
				t.Fatalf("width %d: oversized row %q", width, row)
			}
			all.WriteString(ansi.Strip(row))
		}
		compact := strings.NewReplacer(" ", "", "│", "", "─", "", "┼", "").Replace(all.String())
		// Horizontal layouts interleave cells by row. Compare rune counts rather
		// than requiring row-major order for cells of different heights.
		const content = "NameValue界alphabetaABCDEFe\u0301👩‍🔬"
		for _, r := range content {
			if strings.Count(compact, string(r)) != strings.Count(content, string(r)) {
				t.Fatalf("width %d: content lost/duplicated: %q", width, compact)
			}
		}
	}
}

func TestFlowTokensCanonicalSGR(t *testing.T) {
	input := "\x1b[1;31mA \x1b[22;32mB \x1b[1;38;2;12;34;56mC " +
		"\x1b[0;4:3;58;2;7;8;9mD \x1b[24;39;59mE " +
		"\x1b[1;31mF \x1b[0;34mG \x1b[mH \x1b[31mI \x1b[0mJ"
	want := map[string]uv.Style{
		"A": {Attrs: uv.AttrBold, Fg: ansi.Red},
		"B": {Fg: ansi.Green},
		"C": {Attrs: uv.AttrBold, Fg: ansi.RGBColor{R: 12, G: 34, B: 56}},
		"D": {Underline: uv.UnderlineStyleCurly, UnderlineColor: ansi.RGBColor{R: 7, G: 8, B: 9}},
		"E": {},
		"F": {Attrs: uv.AttrBold, Fg: ansi.Red},
		"G": {Fg: ansi.Blue},
		"H": {},
		"I": {Fg: ansi.Red},
		"J": {},
	}
	seen := 0
	for token := range flowTokens(input) {
		var style uv.Style
		var parser ansi.Parser
		parser.SetParamsSize(32)
		var state byte
		text := token.text
		for len(text) > 0 {
			seq, w, n, next := ansi.DecodeSequence(text, state, &parser)
			text, state = text[n:], next
			if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
				uv.ReadStyle(parser.Params(), &style)
			} else if w > 0 && seq != " " {
				expected, ok := want[seq]
				if !ok || !style.Equal(&expected) {
					t.Fatalf("%q style = %#v; want %#v (token %q)", seq, style, expected, token.text)
				}
				seen++
			}
		}
		if !style.IsZero() {
			t.Fatalf("token leaked style: %q", token.text)
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d characters; want %d", seen, len(want))
	}
}

func TestFlowTokensStyleGrowthIsLinear(t *testing.T) {
	previous := 0
	for _, count := range []int{100, 1000, 10000} {
		// No full resets: colors and intensity are repeatedly overwritten.
		input := strings.Repeat("\x1b[1;31ma \x1b[22;32mb ", count)
		size := 0
		for token := range flowTokens(input) {
			size += len(token.text)
			if len(token.text) > 64 {
				t.Fatalf("%d repetitions: unbounded token prefix (%d bytes)", count, len(token.text))
			}
		}
		if size > 8*len(input) || previous > 0 && size > 11*previous {
			t.Fatalf("%d repetitions: output grew superlinearly: %d bytes (previous %d)", count, size, previous)
		}
		previous = size
	}
}

func BenchmarkFlowTokensRepeatedStyles(b *testing.B) {
	for _, size := range []struct {
		name  string
		count int
	}{{"1k", 1000}, {"10k", 10000}} {
		b.Run(size.name, func(b *testing.B) {
			input := strings.Repeat("\x1b[1;31ma \x1b[22;32mb ", size.count)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				for range flowTokens(input) {
				}
			}
		})
	}
}
