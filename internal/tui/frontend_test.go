package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"ttc/internal/capture"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"

	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/vt"
)

func TestClickSystemPlaceholderOpensSharedWindow(t *testing.T) {
	if _, err := scratch.Verify(); err != nil {
		t.Fatal(err)
	}
	store, e := history.Open(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	w, e := workspace.Open(t.TempDir(), store)
	if e != nil {
		t.Fatal(e)
	}

	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	catalog, _ := skills.Discover(context.Background(), w.Root, "")
	events := make(chan session.Event, 64)
	r, e := session.New(context.Background(), store, w, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Done."}}}, selection, "", catalog, tool.WebSearchConfig{}, func(v session.Event) { events <- v })
	if e != nil {
		t.Fatal(e)
	}
	r.AutoName = false
	defer r.Close()
	screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := Frontend{Runtime: r, Events: events, Screen: screen, Output: io.Discard}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("frontend did not stop")
		}
	}()

	latest := ""
	wait := func(needle string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case latest = <-screen.frames:
			case <-time.After(time.Millisecond):
			}
			if strings.Contains(latest, needle) {
				return
			}
		}
		t.Fatalf("screen missing %q: %s", needle, latest)
	}
	wait("/help")
	screen.SetSize(100, 30)
	screen.EventQ() <- tcell.NewEventResize(100, 30)
	wait("History database:")
	for _, ch := range "hello" {
		screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, string(ch), 0)
	}
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", 0)
	wait("Turn complete")
	if strings.Contains(latest, "You are TTC") {
		t.Fatal("system body leaked into main conversation")
	}
	placeholderY := -1
	for y, row := range strings.Split(latest, "\n") {
		if strings.Contains(row, "System prompt") {
			placeholderY = y
			break
		}
	}
	if placeholderY < 0 {
		t.Fatal("system prompt placeholder not visible", latest)
	}
	screen.EventQ() <- tcell.NewEventMouse(3, placeholderY, tcell.Button1, 0)
	wait("You are TTC")
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyEscape, "", 0)
	wait("System prompt")
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyUp, "", tcell.ModAlt)
	screen.EventQ() <- tcell.NewEventKey(tcell.KeyTab, "", 0)
	wait("Message metadata:")
}

func TestSessionSwitchDrainsBackgroundCompletionEvents(t *testing.T) {
	if _, err := scratch.Verify(); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}

	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 1)
	r, err := session.New(ctx, store, w, &llm.Script{}, selection, "", catalog, tool.WebSearchConfig{}, func(v session.Event) {
		select {
		case events <- v:
		case <-ctx.Done():
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.AutoName = false
	defer func() { cancel(); r.Close() }()
	closing, release := make(chan struct{}), make(chan struct{})
	notify := r.Jobs.Notify
	r.Jobs.Notify = func(v jobs.Snapshot) {
		close(closing)
		<-release
		notify(v)
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if _, err := r.Jobs.Start("main", "sleep 30", w.Root, 0, true, true, false, capture.OutputOptions{}, ""); err != nil {
		t.Fatal(err)
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	output := make(chan string, 16)
	f := Frontend{Runtime: r, Events: events, Input: input, Output: eventWriter{output}, Plain: true}
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("frontend did not stop")
		}
	}()
	if _, err := io.WriteString(writer, "/new\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closing:
	case <-time.After(3 * time.Second):
		t.Fatal("session switch did not cancel background job")
	}
	// The old synchronous frontend cannot drain this slot while joining the callback.
	events <- session.Event{Kind: "status", Text: "queued event", Generation: r.Generation()}
	close(release)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case text := <-output:
			if strings.Contains(text, "New session") {
				return
			}
		case <-deadline:
			t.Fatal("session switch deadlocked on a full event channel")
		}
	}
}

type eventWriter struct{ output chan<- string }

func (w eventWriter) Write(p []byte) (int, error) {
	w.output <- string(p)
	return len(p), nil
}

// testScreen uses an in-memory terminal and records cursor requests for draw assertions.
type testScreen struct {
	tcell.Screen
	term             vt.MockTerm
	cursorX, cursorY int
	cursorVisible    bool
}

func newTestScreen(t *testing.T) *testScreen {
	t.Helper()
	term := vt.NewMockTerm(vt.MockOptSize{X: 80, Y: 25})
	// Unit fixtures inject events directly; protocol negotiation is covered by PTYs.
	screen, err := tcell.NewTerminfoScreenFromTty(term, tcell.OptTerm("xterm"), tcell.OptColors(1<<24), tcell.OptNegotiation(false))
	if err != nil {
		t.Fatal(err)
	}
	return &testScreen{Screen: screen, term: term}
}

func (s *testScreen) SetSize(width, height int) {
	s.term.SetSize(vt.Coord{X: vt.Col(width), Y: vt.Row(height)})
	s.Screen.SetSize(width, height)
}

func (s *testScreen) ShowCursor(x, y int) {
	s.cursorX, s.cursorY, s.cursorVisible = x, y, x >= 0 && y >= 0
	s.Screen.ShowCursor(x, y)
}

func (s *testScreen) HideCursor() {
	s.cursorVisible = false
	s.Screen.HideCursor()
}

func screenText(s tcell.Screen) string {
	width, height := s.Size()
	var b strings.Builder
	for y := range height {
		for x := 0; x < width; {
			text, _, cells := s.Get(x, y)
			b.WriteString(text)
			x += max(1, cells)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// observedScreen snapshots frames on the frontend goroutine to avoid concurrent cell access.
type observedScreen struct {
	tcell.Screen
	frames       chan string
	mouseEnabled atomic.Bool
}

func (s *observedScreen) Show() {
	s.Screen.Show()
	select {
	case s.frames <- screenText(s.Screen):
	default:
	}
}

func (s *observedScreen) EnableMouse(flags ...tcell.MouseFlags) {
	s.Screen.EnableMouse(flags...)
	s.mouseEnabled.Store(true)
}

func (s *observedScreen) DisableMouse() {
	s.Screen.DisableMouse()
	s.mouseEnabled.Store(false)
}
