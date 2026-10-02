package tui

import (
	"errors"
	"strings"
	"testing"

	"scicode/internal/history"

	"github.com/gdamore/tcell/v2"
)

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
