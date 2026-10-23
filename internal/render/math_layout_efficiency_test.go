package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

func TestInlineItemsAdjacentFormulasLazyAndMemoized(t *testing.T) {
	for _, count := range []int{100, 1000, 5000} {
		calls := 0
		boxes := newMarkdownBoxes("", 37, func(tex string, display bool, width int) FixedBox {
			calls++
			return FixedBox{Rows: []string{"M"}, Width: 1}
		})
		boxes.formulas['\ue001'] = formulaToken{tex: "x"}
		items := boxes.inlineItems(strings.Repeat("\ue001", count), false)
		if calls != 0 || len(items) != count {
			t.Fatalf("%d formulas: eager callbacks=%d, items=%d", count, calls, len(items))
		}
		for _, item := range items {
			if item.box == nil || item.preferred != 0 {
				t.Fatalf("unexpected prose item: %+v", item)
			}
		}
		rows := layoutFlow(items, 37)
		if got := strings.Join(rows, ""); got != strings.Repeat("M", count) {
			t.Fatalf("%d formulas: output changed (%d bytes)", count, len(got))
		}
		layoutFlow(items, 37)
		if calls != count {
			t.Fatalf("%d formulas: repeated layout made %d callbacks", count, calls)
		}
		layoutFlow(items, 19)
		layoutFlow(items, 19)
		if calls != 2*count {
			t.Fatalf("%d formulas: second width made %d callbacks", count, calls)
		}
	}
}

func TestInlineItemsCanonicalStyleAndNewlines(t *testing.T) {
	boxes := newMarkdownBoxes("", 80, func(string, bool, int) FixedBox {
		return FixedBox{Rows: []string{"M"}, Width: 1}
	})
	boxes.formulas['\ue001'] = formulaToken{tex: "x"}
	input := "\x1b[1;31mbefore\ue001after\nnext\x1b[22;32m\ue001green\x1b[0m plain"
	items := boxes.inlineItems(input, false)
	if len(items) != 5 {
		t.Fatalf("items=%d; want text/box/text/box/text", len(items))
	}
	if ansi.Strip(items[0].text) != "before" || ansi.Strip(items[2].text) != "after\nnext" || ansi.Strip(items[4].text) != "green plain" {
		t.Fatalf("fragments lost text or newline: %#v", items)
	}
	if !strings.HasPrefix(items[2].text, "\x1b[1;31m") || !strings.HasPrefix(items[4].text, "\x1b[32m") {
		t.Fatalf("canonical styles not replayed: %q / %q", items[2].text, items[4].text)
	}
	if got := strings.Join(plainBoxRows(layoutFlow(items, 80)), "\n"); got != "beforeMafter\nnextMgreen plain" {
		t.Fatalf("layout changed text/newlines: %q", got)
	}
	// The marker and combining mark can occupy the same decoded grapheme.
	items = boxes.inlineItems("\ue001\u0301tail", false)
	if len(items) != 2 || items[1].text != "\u0301tail" {
		t.Fatalf("combining suffix lost: %#v", items)
	}
}

func TestInlineItemsTablePreferredMemoized(t *testing.T) {
	calls := 0
	boxes := newMarkdownBoxes("", 40, func(string, bool, int) FixedBox {
		calls++
		return FixedBox{Rows: []string{"xx"}, Width: 2}
	})
	boxes.formulas['\ue001'] = formulaToken{tex: "x"}
	items := boxes.inlineItems("\ue001\ue001", true)
	if calls != 2 || len(items) != 2 {
		t.Fatalf("preferred measurements: calls=%d, items=%d", calls, len(items))
	}
	for _, item := range items {
		if item.preferred != 2 {
			t.Fatalf("preferred=%d; want 2", item.preferred)
		}
		item.box(40)
		item.box(40)
		item.box(0)
		item.box(1)
	}
	if calls != 4 {
		t.Fatalf("expected one callback per occurrence and normalized width, got %d", calls)
	}
}

func TestInlinePreferredOnlyForTableCells(t *testing.T) {
	for _, node := range []ast.Node{ast.NewParagraph(), ast.NewHeading(2), extast.NewTableCell()} {
		t.Run(node.Kind().String(), func(t *testing.T) {
			calls := 0
			boxes := newMarkdownBoxes("", 40, func(string, bool, int) FixedBox {
				calls++
				return FixedBox{Rows: []string{"M"}, Width: 1}
			})
			boxes.formulas['\ue001'] = formulaToken{tex: "x"}
			source := []byte("\ue001")
			node.AppendChild(node, ast.NewTextSegment(text.NewSegment(0, len(source))))
			items, err := boxes.inline(node, source)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, item := range items {
				if item.box != nil {
					count++
				}
			}
			want := 0
			if node.Kind() == extast.KindTableCell {
				want = 1
			}
			if count != 1 || calls != want {
				t.Fatalf("boxes=%d, eager callbacks=%d; want 1 and %d", count, calls, want)
			}
		})
	}
}

func BenchmarkInlineItemsAdjacentFormulas(b *testing.B) {
	for _, size := range []struct {
		name  string
		count int
	}{{"1k", 1000}, {"10k", 10000}} {
		b.Run(size.name, func(b *testing.B) {
			boxes := newMarkdownBoxes("", 80, func(string, bool, int) FixedBox {
				return FixedBox{Rows: []string{"M"}, Width: 1}
			})
			boxes.formulas['\ue001'] = formulaToken{tex: "x"}
			input := strings.Repeat("\ue001", size.count)
			b.SetBytes(int64(len(input)))
			b.ReportAllocs()
			for b.Loop() {
				boxes.inlineItems(input, false)
			}
		})
	}
}
