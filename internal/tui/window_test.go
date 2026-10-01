package tui

import (
	"scicode/internal/provider"
	"scicode/internal/render"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestSharedWindowWrapAndScroll(t *testing.T) {
	w := &Window{Title: "System prompt", Text: "exact\n" + strings.Repeat("界", 10) + "\nlast"}
	lines := w.Lines(5, 2)
	if len(lines) != 2 || lines[0] != "exact" {
		t.Fatal(lines)
	}
	w.Key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 2)
	lines = w.Lines(5, 2)
	if lines[len(lines)-1] != "last" {
		t.Fatal(lines)
	}
	w.Key(tcell.NewEventKey(tcell.KeyHome, 0, 0), 2)
	if w.Scroll != 0 {
		t.Fatal(w.Scroll)
	}
	w.Key(tcell.NewEventKey(tcell.KeyCtrlD, 0, 0), 4)
	if w.Scroll != 2 {
		t.Fatal("Ctrl+D did not scroll down", w.Scroll)
	}
}
func TestNarrowScreenDraw(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if e := s.Init(); e != nil {
		t.Fatal(e)
	}
	defer s.Fini()
	s.SetSize(20, 8)
	draw(s, transcriptOf([]line{{text: "System prompt · inspect", id: 1}}), newSidebar(), false, nil, nil, 0, newComposer("draft"), 0, nil, false, time.Time{}, &Window{Title: "Inspector", Text: "hello\x1bworld"}, provider.Selection{})
	cells, _, _ := s.GetContents()
	if len(cells) != 160 {
		t.Fatal(len(cells))
	}
}

func TestBorderedWindowProgressTopMiddleBottomAndResize(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 16)
	w := &Window{Title: "System prompt", Text: strings.TrimSuffix(strings.Repeat("row\n", 100), "\n"), System: true}
	frame := func() string {
		draw(s, newTranscript(), newSidebar(), false, nil, nil, -1, newComposer(""), 0, nil, false, time.Time{}, w, provider.Selection{})
		cells, width, height := s.GetContents()
		var out strings.Builder
		for y := range height {
			for x := range width {
				for _, r := range cells[y*width+x].Runes {
					out.WriteRune(r)
				}
			}
			out.WriteByte('\n')
		}
		return out.String()
	}
	if text := frame(); !strings.Contains(text, "┌System prompt") || !strings.Contains(text, "0%") || !strings.Contains(text, "lines 1–") {
		t.Fatal(text)
	}
	w.Scroll = 44
	if text := frame(); !strings.Contains(text, "50%") {
		t.Fatal(text)
	}
	w.Key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 10)
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
				r, _, _, _ := s.GetContent(cell.x, cell.y)
				if r != cell.want {
					t.Fatalf("tiny border overwritten at %d,%d: %q", cell.x, cell.y, r)
				}
			}
		}
	}
}

func TestHumanBackgroundSystemColorAndQueueRows(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(60, 12)
	lines := []line{{text: "user instructions", id: 1, human: true}, {text: "System prompt · inspect", id: 2, system: true}}
	queue := []provider.Message{{Content: "first queued\nsecond part"}, {Content: strings.Repeat("long queued ", 20)}}
	view := transcriptOf(lines)
	draw(s, view, newSidebar(), false, nil, nil, -1, newComposer("draft"), 0, queue, true, time.Now(), nil, provider.Selection{})
	_, _, style, _ := s.GetContent(1, 0)
	_, bg, _ := style.Decompose()
	if bg != tcell.GetColor(render.HumanColor) {
		t.Fatal("human instruction background missing", bg)
	}
	_, _, style, _ = s.GetContent(1, 1)
	fg, _, _ := style.Decompose()
	if fg != tcell.GetColor(render.LavenderColor) {
		t.Fatal("system color missing", fg)
	}
	row := func(y int) string {
		var out strings.Builder
		for x := range 60 {
			r, _, _, _ := s.GetContent(x, y)
			out.WriteRune(r)
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
