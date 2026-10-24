package tui

import (
	"strings"
	"testing"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/render"

	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/color"
)

func TestSharedWindowWrapAndScroll(t *testing.T) {
	w := &Window{Title: "System prompt", Text: "exact\n" + strings.Repeat("界", 10) + "\nlast"}
	lines := w.Lines(5, 2)
	if len(lines) != 2 || lines[0] != "exact" {
		t.Fatal(lines)
	}
	w.Key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 2)
	lines = w.Lines(5, 2)
	if lines[len(lines)-1] != "last" {
		t.Fatal(lines)
	}
	w.Key(tcell.NewEventKey(tcell.KeyHome, "", 0), 2)
	if w.Scroll != 0 {
		t.Fatal(w.Scroll)
	}
	w.Key(tcell.NewEventKey(tcell.KeyCtrlD, "", 0), 4)
	if w.Scroll != 2 {
		t.Fatal("Ctrl+D did not scroll down", w.Scroll)
	}
}

func TestLiveWindowTextFollowsOnlyAtBottom(t *testing.T) {
	for _, atBottom := range []bool{true, false} {
		w := &Window{Text: "one\ntwo\nthree\nfour\nfive\nsix"}
		w.Lines(20, 3)
		w.Key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 3)
		w.Lines(20, 3)
		if !atBottom {
			w.Key(tcell.NewEventKey(tcell.KeyUp, "", 0), 3)
		}
		oldScroll := w.Scroll
		w.updateLiveText(w.Text+"\nnew-output", 20, 3)
		rows := w.Lines(20, 3)
		if atBottom {
			if rows[len(rows)-1] != "new-output" || w.Scroll != oldScroll+1 {
				t.Fatalf("bottom viewport did not follow: offset=%d rows=%q", w.Scroll, rows)
			}
		} else if w.Scroll != oldScroll || strings.Contains(strings.Join(rows, "\n"), "new-output") {
			t.Fatalf("manual upward scroll was lost: offset=%d rows=%q", w.Scroll, rows)
		}
		w.updateLiveText(w.Text, 20, 3)
		wantScroll := oldScroll
		if atBottom {
			wantScroll++
		}
		if w.Scroll != wantScroll {
			t.Fatal("unchanged text moved viewport", w.Scroll)
		}
	}
}
func TestNarrowScreenDraw(t *testing.T) {
	s := newTestScreen(t)
	if e := s.Init(); e != nil {
		t.Fatal(e)
	}
	defer s.Fini()
	s.SetSize(20, 8)
	draw(s, transcriptOf([]line{{text: "System prompt · inspect", id: 1}}), newSidebar(), false, nil, nil, 0, newComposer("draft"), 0, nil, nil, "", &Window{Title: "Inspector", Text: "hello\x1bworld"}, llm.Selection{})
	width, height := s.Size()
	if width*height != 160 {
		t.Fatal(width, height)
	}
}

func TestBorderedWindowProgressTopMiddleBottomAndResize(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 16)
	w := &Window{Title: "System prompt", Text: strings.TrimSuffix(strings.Repeat("row\n", 100), "\n"), System: true}
	frame := func() string {
		draw(s, newTranscript(), newSidebar(), false, nil, nil, -1, newComposer(""), 0, nil, nil, "", w, llm.Selection{})
		return screenText(s)
	}
	if text := frame(); !strings.Contains(text, "┌System prompt") || !strings.Contains(text, "0%") || !strings.Contains(text, "lines 1–") {
		t.Fatal(text)
	}
	w.Scroll = 44
	if text := frame(); !strings.Contains(text, "50%") {
		t.Fatal(text)
	}
	w.Key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 10)
	if text := frame(); !strings.Contains(text, "100%") {
		t.Fatal(text)
	}
	s.SetSize(12, 8)
	if text := frame(); !strings.Contains(text, "100%") {
		t.Fatal("progress missing in narrow pane", text)
	}
	for _, size := range [][2]int{{1, 1}, {2, 2}, {4, 3}} {
		s.SetSize(size[0], size[1])
		frame()
		if size[0] >= 2 && size[1] >= 2 {
			for _, cell := range []struct {
				x, y int
				want rune
			}{{0, 0, '┌'}, {size[0] - 1, 0, '┐'}, {0, size[1] - 1, '└'}, {size[0] - 1, size[1] - 1, '┘'}} {
				r, _, _ := s.Get(cell.x, cell.y)
				if r != string(cell.want) {
					t.Fatalf("tiny border overwritten at %d,%d: %q", cell.x, cell.y, r)
				}
			}
		}
	}
}

func TestHumanBackgroundSystemColorAndQueueRows(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 12)
	lines := []line{{text: "user instructions", id: 1, human: true}, {text: "System prompt · inspect", id: 2, system: true}}
	queue := []contextbuild.Input{{Text: "first queued\nsecond part"}, {Text: strings.Repeat("long queued ", 20)}}
	view := transcriptOf(lines)
	draw(s, view, newSidebar(), false, nil, nil, -1, newComposer("draft"), 0, queue, nil, "Working · 0s", nil, llm.Selection{})
	_, style, _ := s.Get(1, 0)
	bg := style.GetBackground()
	if bg != color.GetColor(render.HumanColor) {
		t.Fatal("human instruction background missing", bg)
	}
	_, style, _ = s.Get(1, 1)
	fg := style.GetForeground()
	if fg != color.GetColor(render.LavenderColor) {
		t.Fatal("system color missing", fg)
	}
	row := func(y int) string {
		var out strings.Builder
		for x := range 60 {
			r, _, _ := s.Get(x, y)
			out.WriteString(r)
		}
		return out.String()
	}
	if !strings.Contains(row(7), "Working") || !strings.Contains(row(8), "Queued · first queued second part") || !strings.Contains(row(9), "Queued · long queued") || !strings.Contains(row(10), "> draft") {
		t.Fatal(row(7), row(8), row(9), row(10))
	}
	if id := view.entryAt(1); id != 2 {
		t.Fatal("queue shifted inspector hit test", id)
	}
	if id := view.entryAt(8); id != 0 {
		t.Fatal("queue row opened a conversation message", id)
	}
}
