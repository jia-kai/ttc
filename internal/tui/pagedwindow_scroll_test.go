package tui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/history"
)

func scrollPageFixture(texts []string) (*Window, *[]int) {
	reads := []int{}
	load := func(offset int) (history.InspectionPage, error) {
		reads = append(reads, offset)
		return history.InspectionPage{Text: texts[offset/100], Offset: offset, Limit: 100, Total: len(texts) * 100, Supported: true}, nil
	}
	page, _ := load(0)
	return NewPagedWindow(page, load), &reads
}

func TestPagedWindowContinuedKeyboardScroll(t *testing.T) {
	for _, keys := range []struct {
		name     string
		down, up tcell.Key
	}{
		{"arrows", tcell.KeyDown, tcell.KeyUp},
		{"pages", tcell.KeyPgDn, tcell.KeyPgUp},
		{"half pages", tcell.KeyCtrlD, tcell.KeyCtrlU},
	} {
		t.Run(keys.name, func(t *testing.T) {
			body := strings.TrimSuffix(strings.Repeat("row\n", 10), "\n")
			w, reads := scrollPageFixture([]string{body, body, body})
			w.Lines(20, 3)
			for w.Scroll < 7 {
				w.Key(tcell.NewEventKey(keys.down, "", 0), 3)
			}
			if w.pages.page.Offset != 0 || len(*reads) != 1 {
				t.Fatal("reaching bottom loaded a page", *reads)
			}
			w.Key(tcell.NewEventKey(keys.down, "", 0), 3)
			if w.pages.page.Offset != 100 || w.Scroll != 0 {
				t.Fatal("next page must start at top", w.Scroll, *reads)
			}
			w.Key(tcell.NewEventKey(keys.up, "", 0), 3)
			if w.pages.page.Offset != 0 || w.Scroll != 7 {
				t.Fatal("previous page must end at bottom", w.Scroll, *reads)
			}
			if !reflect.DeepEqual(*reads, []int{0, 100, 0}) {
				t.Fatal(*reads)
			}
		})
	}
}

func TestPagedWindowKeyboardWithNoBodyDoesNotLoad(t *testing.T) {
	w, reads := scrollPageFixture([]string{"short", "next"})
	w.Lines(20, 0)
	for _, height := range []int{0, -1} {
		for _, key := range []tcell.Key{tcell.KeyDown, tcell.KeyUp, tcell.KeyPgDn, tcell.KeyPgUp, tcell.KeyCtrlD, tcell.KeyCtrlU} {
			if !w.Key(tcell.NewEventKey(key, "", 0), height) || len(*reads) != 1 || w.pages.page.Offset != 0 {
				t.Fatalf("invisible viewport loaded content: height=%d key=%v reads=%v", height, key, *reads)
			}
		}
	}
	w.Key(tcell.NewEventKey(tcell.KeyRune, "]", 0), 0)
	if len(*reads) != 2 {
		t.Fatal("explicit page key was disabled with zero body height")
	}
}

func TestPagedWindowScrollLimitsShortEmptyAndFailure(t *testing.T) {
	w, reads := scrollPageFixture([]string{"abcdefghij", "", "short", "final"})
	w.Lines(2, 2) // Five wrapped rows, not one source row.
	huge := int(^uint(0) >> 1)
	if w.ScrollBy(huge, 2) || w.Scroll != 3 || len(*reads) != 1 {
		t.Fatal("overscroll skipped first boundary", w.Scroll, *reads)
	}
	if !w.ScrollBy(huge, 2) || w.pages.page.Offset != 100 || len(*reads) != 2 {
		t.Fatal("empty page skipped", *reads)
	}
	if !w.ScrollBy(huge, 2) || w.pages.page.Offset != 200 || len(*reads) != 3 {
		t.Fatal("short page skipped", *reads)
	}
	if !w.ScrollBy(-huge, 2) || w.pages.page.Offset != 100 || w.Scroll != 0 {
		t.Fatal("reverse empty page", w.Scroll, *reads)
	}
	if !w.ScrollBy(-huge, 2) || w.pages.page.Offset != 0 || w.Scroll != 3 {
		t.Fatal("reverse must use wrapped rows", w.Scroll, *reads)
	}
	w.Key(tcell.NewEventKey(tcell.KeyHome, "", 0), 2)
	if w.ScrollBy(-huge, 2) || w.Scroll != 0 || len(*reads) != 5 {
		t.Fatal("past start", *reads)
	}
	for _, delta := range []int{0, 1, -1} {
		w.ScrollBy(delta, 0)
	}
	if len(*reads) != 5 {
		t.Fatal("zero height loaded pages")
	}
	w.Scroll = huge
	for _, width := range []int{1, 10, 2} {
		w.Lines(width, 2)
	}
	if len(*reads) != 5 {
		t.Fatal("draw loaded pages")
	}
	w.Key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 2)
	w.pages.load = func(int) (history.InspectionPage, error) {
		return history.InspectionPage{}, errors.New("read-only page unavailable")
	}
	if w.ScrollBy(1, 2) || w.pages.page.Offset != 0 || w.Text != "abcdefghij" || w.Scroll != 3 || !strings.Contains(w.Header, "read-only page unavailable") {
		t.Fatal("failed load changed page", w.Text, w.Scroll, w.Header)
	}
}

func TestPagedWindowHomeEndBracketsAndTerminalPage(t *testing.T) {
	w, reads := scrollPageFixture([]string{"first", "last"})
	w.Lines(40, 10)
	w.Key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 10)
	w.Lines(40, 10)
	w.Key(tcell.NewEventKey(tcell.KeyHome, "", 0), 10)
	if len(*reads) != 1 {
		t.Fatal("Home/End changed page")
	}
	w.Key(tcell.NewEventKey(tcell.KeyRune, "]", 0), 10)
	for range 3 {
		w.ScrollBy(100, 10)
	}
	if len(*reads) != 2 || w.pages.page.Offset != 100 || w.Scroll != 0 {
		t.Fatal("past terminal page", *reads, w.Scroll)
	}
	w.Key(tcell.NewEventKey(tcell.KeyRune, "[", 0), 10)
	if w.pages.page.Offset != 0 || w.Scroll != 0 {
		t.Fatal("explicit previous page must start at top")
	}
}

func TestControllerPagedWindowWheelUsesBodyViewportAndResize(t *testing.T) {
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(30, 12)
	c := pasteTestController()
	c.screen = screen
	body := strings.TrimSuffix(strings.Repeat("row\n", 20), "\n")
	w, reads := scrollPageFixture([]string{body, body, body})
	w.Header = "fixed header\nsecond header"
	w.SourceAgent = "test"
	c.modal.window = w
	width, height := windowContentSize(30, 12, w)
	if height >= 10 {
		t.Fatal("fixture needs a fixed header", height)
	}
	// No preceding draw: the controller must prime the width itself.
	for range 20 - height {
		c.handleMouse(tcell.NewEventMouse(1, 1, tcell.WheelDown, 0))
		if w.Scroll == 20-height {
			break
		}
	}
	if len(*reads) != 1 || w.Scroll != 20-height {
		t.Fatal("wheel reached boundary too early", w.Scroll, height, *reads)
	}
	c.handleMouse(tcell.NewEventMouse(1, 1, tcell.WheelDown, 0))
	if w.pages.page.Offset != 100 || w.Scroll != 0 || w.cachedWidth != width {
		t.Fatal("wheel did not turn page", *reads, w.Scroll)
	}
	c.handleMouse(tcell.NewEventMouse(1, 1, tcell.WheelUp, 0))
	_, height = windowContentSize(30, 12, w)
	if w.pages.page.Offset != 0 || w.Scroll != 20-height {
		t.Fatal("reverse wheel ignored body viewport", *reads, w.Scroll, height)
	}
	// A narrower pane changes wrapping before the next input, even without draw.
	screen.SetSize(4, 12)
	c.handleMouse(tcell.NewEventMouse(1, 1, tcell.WheelDown, 0))
	if len(*reads) != 3 || w.pages.page.Offset != 0 {
		t.Fatal("resize used stale wrapping", *reads)
	}
}

func TestNonpagedScrollDoesNotAcquirePager(t *testing.T) {
	w := &Window{Text: "fixed"}
	w.Lines(20, 10)
	if w.ScrollBy(100, 10) || w.Scroll != 100 {
		t.Fatal("nonpaged scroll behavior changed", w.Scroll)
	}
	w.Lines(20, 10)
	w.Key(tcell.NewEventKey(tcell.KeyDown, "", 0), 10)
	if w.Scroll != 1 || w.pages != nil {
		t.Fatal("nonpaged key behavior changed")
	}
	w.Lines(20, 10)
	if w.Scroll != 0 {
		t.Fatal("nonpaged draw must clamp")
	}
}

func TestControllerPagedKeyboardPrimesCurrentWidth(t *testing.T) {
	screen := newTestScreen(t)
	if err := screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(30, 12)
	c := pasteTestController()
	c.screen = screen
	w, reads := scrollPageFixture([]string{strings.Repeat("x", 1000), "last"})
	w.Header = "fixed metadata"
	c.modal.window = w
	width, height := windowContentSize(30, 12, w)
	c.handleKey(tcell.NewEventKey(tcell.KeyPgDn, "", 0))
	if w.cachedWidth != width || w.Scroll != height || len(*reads) != 1 {
		t.Fatal("keyboard used stale width or viewport", w.cachedWidth, w.Scroll, height, *reads)
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyEnd, "", 0))
	if len(*reads) != 1 {
		t.Fatal("End turned a page")
	}
	c.handleKey(tcell.NewEventKey(tcell.KeyCtrlD, "", 0))
	if w.pages.page.Offset != 100 || w.Scroll != 0 || len(*reads) != 2 {
		t.Fatal("continued keyboard scroll did not turn page", w.Scroll, *reads)
	}
}
