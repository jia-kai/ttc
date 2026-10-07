package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"

	"github.com/gdamore/tcell/v2"
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
	screen := &observedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), frames: make(chan string, 64)}
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
	screen.PostEventWait(tcell.NewEventResize(100, 30))
	for _, ch := range "hello" {
		screen.InjectKey(tcell.KeyRune, ch, 0)
	}
	screen.InjectKey(tcell.KeyEnter, 0, 0)
	wait("Turn complete")
	if strings.Contains(latest, "You are TTC") {
		t.Fatal("system body leaked into main conversation")
	}
	screen.InjectMouse(3, 2, tcell.Button1, 0)
	wait("You are TTC")
	screen.InjectKey(tcell.KeyEscape, 0, 0)
	wait("System prompt")
	screen.InjectKey(tcell.KeyUp, 0, tcell.ModAlt)
	screen.InjectKey(tcell.KeyTab, 0, 0)
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
	if _, err := r.Jobs.Start("main", "sleep 30", w.Root, 0, true, true, false); err != nil {
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

// observedScreen snapshots frames on the frontend goroutine, avoiding tcell's mutable GetContents slice.
type observedScreen struct {
	tcell.SimulationScreen
	frames       chan string
	mouseEnabled atomic.Bool
}

func (s *observedScreen) Show() {
	s.SimulationScreen.Show()
	cells, width, _ := s.SimulationScreen.GetContents()
	var b strings.Builder
	for i, c := range cells {
		for _, r := range c.Runes {
			b.WriteRune(r)
		}
		if width > 0 && (i+1)%width == 0 {
			b.WriteByte('\n')
		}
	}
	select {
	case s.frames <- b.String():
	default:
	}
}

func (s *observedScreen) EnableMouse(flags ...tcell.MouseFlags) {
	s.SimulationScreen.EnableMouse(flags...)
	s.mouseEnabled.Store(true)
}

func (s *observedScreen) DisableMouse() {
	s.SimulationScreen.DisableMouse()
	s.mouseEnabled.Store(false)
}
