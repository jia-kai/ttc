package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"scicode/internal/history"
	"scicode/internal/render"

	"github.com/gdamore/tcell/v2"
)

func TestPagedWindowKeepsCapturedOutputLiteral(t *testing.T) {
	for _, fence := range []string{"````", "~~~", "  `````"} {
		t.Run(fence, func(t *testing.T) {
			source := []rune(fence + "text\n```\n~~~not-a-closer\n````not-a-closer\n" +
				strings.Repeat("literal $x^2$ and **Markdown**\n", 1400) + fence + "\n")
			load := func(offset int) (history.InspectionPage, error) {
				return history.InspectionPage{Title: "Captured output", Text: string(source[offset:min(len(source), offset+history.InspectionPageChars)]), Offset: offset, Limit: history.InspectionPageChars, Total: len(source), Supported: true, Markdown: true}, nil
			}
			first, _ := load(0)
			w := NewPagedWindow(first, load)
			check := func() {
				t.Helper()
				formulas := 0
				render.Math(w.Text, func(string, bool) string { formulas++; return "FORMULA" })
				if formulas != 0 {
					t.Fatal("captured code became math after paging", w.pages.page.Offset, formulas)
				}
				for _, width := range []int{40, 100} {
					w.Lines(width, 20)
					if !strings.Contains(strings.Join(w.cachedLines, "\n"), "$x^2$") {
						t.Fatal("literal job output disappeared", w.pages.page.Offset, width)
					}
				}
			}
			check()
			for w.pages.page.Offset+w.pages.page.Limit < w.pages.page.Total {
				w.pageKey(']')
				if !strings.Contains(w.Text, fence+"text\n") {
					t.Fatal("fence delimiter, indentation or language lost", w.Text[:min(len(w.Text), 32)])
				}
				check()
			}
			for w.pages.page.Offset > 0 {
				w.pageKey('[')
				check()
			}
		})
	}
}

func TestPagedWindowPreservesSplitDelimiterRows(t *testing.T) {
	for _, opening := range []string{"~~~text", "````text", "  ````text"} {
		for _, closing := range []bool{false, true} {
			t.Run(opening+"/closing="+fmt.Sprint(closing), func(t *testing.T) {
				delimiter := strings.TrimSuffix(opening, "text")
				prefix := strings.Repeat("a", history.InspectionPageChars-3) + "\n"
				text := prefix + opening + "\n$x^2$\n" + delimiter + "\n"
				want := 0
				if closing {
					prefix = opening + "\n" + strings.Repeat("a", history.InspectionPageChars-len(opening)-4) + "\n"
					text = prefix + delimiter + "\n\n$x^2$\n"
					want = 1 // This formula follows the real closing fence.
				}
				load := func(offset int) (history.InspectionPage, error) {
					return history.InspectionPage{Title: "Split fence", Text: text[offset:min(len(text), offset+history.InspectionPageChars)], Offset: offset, Limit: history.InspectionPageChars, Total: len(text), Supported: true, Markdown: true}, nil
				}
				first, _ := load(0)
				w := NewPagedWindow(first, load)
				w.pageKey(']')
				formulas := 0
				render.Math(w.Text, func(string, bool) string { formulas++; return "FORMULA" })
				if formulas != want {
					t.Fatal("split delimiter changed code protection", opening, closing, formulas, w.Text)
				}
			})
		}
	}
}

func TestPagedWindowBodyFragmentCannotBecomeFence(t *testing.T) {
	text := "````text\n" + strings.Repeat("a", history.InspectionPageChars-len("````text\n")) + "````\n$x^2$\n````\n"
	load := func(offset int) (history.InspectionPage, error) {
		return history.InspectionPage{Text: text[offset:min(len(text), offset+history.InspectionPageChars)], Offset: offset, Limit: history.InspectionPageChars, Total: len(text), Supported: true, Markdown: true}, nil
	}
	first, _ := load(0)
	w := NewPagedWindow(first, load)
	w.pageKey(']')
	formulas := 0
	render.Math(w.Text, func(string, bool) string { formulas++; return "FORMULA" })
	if formulas != 0 {
		t.Fatal("body-row fragment became a closing fence", w.Text)
	}
}

func TestPagedWindowBoundsUnbrokenFenceMetadata(t *testing.T) {
	text := "~~~" + strings.Repeat("info", 40<<10)
	load := func(offset int) (history.InspectionPage, error) {
		return history.InspectionPage{Text: text[offset:min(len(text), offset+history.InspectionPageChars)], Offset: offset, Limit: history.InspectionPageChars, Total: len(text), Supported: true, Markdown: true}, nil
	}
	first, _ := load(0)
	w := NewPagedWindow(first, load)
	for w.pages.page.Offset+w.pages.page.Limit < w.pages.page.Total {
		w.pageKey(']')
		for _, hint := range w.pages.fences {
			if len(hint.partial) > 64<<10 || len(hint.open) > 64<<10 {
				t.Fatal("unbounded fence metadata")
			}
		}
	}
	if w.Markdown || !strings.Contains(w.Header, "plain text") || len(w.Text) > history.InspectionPageChars {
		t.Fatal("oversized fence did not stay readable and bounded", w.Header, len(w.Text))
	}
}

func TestPagedWindowNavigationKeepsBoundedRenderAndResize(t *testing.T) {
	source := strings.Repeat("x", 8<<20)
	reads := 0
	load := func(offset int) (history.InspectionPage, error) {
		reads++
		end := min(offset+history.InspectionPageChars, len(source))
		return history.InspectionPage{Title: "Large tool", Text: source[offset:end], Offset: offset, Total: len(source), Limit: history.InspectionPageChars, Supported: true}, nil
	}
	first, _ := load(0)
	w := NewPagedWindow(first, load)
	for i := 0; i < 20; i++ {
		width := []int{1, 40, 80, 120}[i%4]
		lines := w.Lines(width, 20)
		if len(lines) > 20 || len(w.cachedLines) > history.InspectionPageChars+1 || len(w.Text) > 64<<10 {
			t.Fatal("inspector rendered beyond bounded page", len(lines), len(w.cachedLines), len(w.Text))
		}
	}
	if reads != 1 {
		t.Fatal("resize or redraw refetched complete source", reads)
	}
	if !w.Key(tcell.NewEventKey(tcell.KeyRune, ']', 0), 20) || w.pages.page.Offset != history.InspectionPageChars || w.Scroll != 0 {
		t.Fatal("next page failed")
	}
	w.Lines(80, 20)
	if !strings.Contains(w.Progress(20, 80), "page 2/512") {
		t.Fatal("missing page progress", w.Progress(20, 80))
	}
	w.Key(tcell.NewEventKey(tcell.KeyRune, '[', 0), 20)
	if w.pages.page.Offset != 0 || reads != 3 {
		t.Fatal("previous page failed")
	}
}

func TestPagedWindowPreservesFenceHintsAndFailedPage(t *testing.T) {
	first := history.InspectionPage{Title: "Code", Text: "```go\npackage fixture\n", Limit: 20, Total: 40, Supported: true, Markdown: true}
	w := NewPagedWindow(first, func(offset int) (history.InspectionPage, error) {
		return history.InspectionPage{Title: "Code", Text: "func main() {}\n```", Offset: offset, Limit: 20, Total: 40, Supported: true, Markdown: true}, nil
	})
	w.pageKey(']')
	if !strings.HasPrefix(w.Text, "```go\n") {
		t.Fatal("code language lost across page", w.Text)
	}
	text := w.Text
	w.pages.load = func(int) (history.InspectionPage, error) {
		return history.InspectionPage{}, errors.New("history unavailable")
	}
	w.pageKey('[')
	if w.Text != text || !strings.Contains(w.Header, "history unavailable") {
		t.Fatal("failed page destroyed inspectable content", w.Header)
	}
}

func TestPagedWindowEmptyEnvelopeHasOnePage(t *testing.T) {
	w := NewPagedWindow(history.InspectionPage{Title: "Assistant", Limit: history.InspectionPageChars, Supported: true, LargeEnvelope: true}, nil)
	w.Lines(80, 20)
	if !strings.Contains(w.Progress(20, 80), "page 1/1") {
		t.Fatal("empty canonical message has invalid page count", w.Progress(20, 80))
	}
	if !w.pageKey(']') || w.pages.page.Offset != 0 {
		t.Fatal("empty message attempted to load another page")
	}
}
