package tui

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/catalog"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

type catalogUpdateScreen struct {
	*observedScreen
	shows atomic.Int64
}

func (s *catalogUpdateScreen) Show() {
	s.shows.Add(1)
	s.observedScreen.Show()
}

type catalogUpdateFixture struct {
	screen   *catalogUpdateScreen
	runtime  *session.Runtime
	provider *gatedModelProvider
	updates  chan catalog.Update
	cancel   context.CancelFunc
	done     chan error
}

func newCatalogUpdateFixture(t *testing.T) *catalogUpdateFixture {
	t.Helper()
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &gatedModelProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
	events := make(chan session.Event, 64)
	models := menuModels()
	r, err := session.New(ctx, store, w, p, llm.Selection{Provider: "script", Model: models[0], Variant: "low"}, "", &skills.Catalog{}, tool.WebSearchConfig{}, func(e session.Event) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.AutoName = false
	screen := &catalogUpdateScreen{observedScreen: &observedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), frames: make(chan string, 256)}}
	h := &catalogUpdateFixture{screen: screen, runtime: r, provider: p, updates: make(chan catalog.Update, 1), cancel: cancel, done: make(chan error, 1)}
	f := Frontend{Runtime: r, Events: events, Screen: screen, Output: io.Discard, Models: models, ModelUpdates: h.updates}
	go func() { h.done <- f.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			t.Error("frontend did not stop")
		}
		r.Close()
	})
	h.wait(t, "/help")
	screen.SetSize(120, 36)
	screen.PostEventWait(tcell.NewEventResize(120, 36))
	return h
}

func (h *catalogUpdateFixture) wait(t *testing.T, needle string) string {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	latest := ""
	for {
		select {
		case latest = <-h.screen.frames:
			if strings.Contains(latest, needle) {
				return latest
			}
		case <-deadline.C:
			t.Fatalf("screen missing %q: %s", needle, latest)
		}
	}
}

func (h *catalogUpdateFixture) key(k tcell.Key) { h.screen.InjectKey(k, 0, 0) }
func (h *catalogUpdateFixture) command(text string) {
	for _, ch := range text {
		h.screen.InjectKey(tcell.KeyRune, ch, 0)
	}
	h.key(tcell.KeyEnter)
}
func (h *catalogUpdateFixture) request(t *testing.T) llm.Request {
	t.Helper()
	select {
	case req := <-h.provider.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("request did not arrive")
		return llm.Request{}
	}
}

func TestCatalogRefreshPreservesOpenMenuAndActiveTurn(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	initial := h.runtime.CurrentSelection()
	// No update has been delivered: both input and the cached picker are usable.
	h.command("first")
	first := h.request(t)
	h.screen.InjectKey(tcell.KeyCtrlX, 0, 0)
	h.screen.InjectKey(tcell.KeyRune, 'm', 0)
	h.wait(t, "Family B")
	fresh := menuModels()
	fresh[0].Name, fresh[0].Revision = "Refreshed A", "new-revision"
	fresh[0].Budget.ContextLimit += 1000
	fresh = fresh[:1] // Family B disappears while the old picker is open.
	h.updates <- catalog.Update{Models: fresh}
	close(h.updates)
	// The old menu remains navigable even though its model has disappeared.
	h.key(tcell.KeyDown)
	h.wait(t, "> Family B")
	h.key(tcell.KeyEnter)
	h.wait(t, "Reasoning variant · Family B")
	h.key(tcell.KeyEnter)
	h.wait(t, `unknown model "family-b"`)
	if !reflect.DeepEqual(h.runtime.ModelChoice(), initial) || !reflect.DeepEqual(first.Selection, initial) {
		t.Fatal("refresh or stale-menu submission changed active selection")
	}
	// Release the tool boundary: refresh alone must not affect the next request.
	h.provider.release <- struct{}{}
	second := h.request(t)
	if !reflect.DeepEqual(second.Selection, initial) {
		t.Fatal("refresh changed in-flight turn metadata", second.Selection)
	}
	h.provider.release <- struct{}{}
	h.wait(t, "Turn complete")
	h.command("/model")
	frame := h.wait(t, "Refreshed A")
	if strings.Contains(frame, "Family B") {
		t.Fatal("new picker retained removed model", frame)
	}
	h.key(tcell.KeyEscape)
	h.command("/model family-a low")
	h.wait(t, "Model selected for next tool boundary · family-a · low")
	h.command("next")
	third := h.request(t)
	if !reflect.DeepEqual(third.Selection.Model, fresh[0]) || third.Selection.Variant != "low" {
		t.Fatal("/model did not resolve refreshed metadata", third.Selection)
	}
}

func TestCatalogRefreshFailureRetainsCachedPicker(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	initial := h.runtime.CurrentSelection()
	h.updates <- catalog.Update{Models: []llm.ModelSpec{llm.ScriptModel()}, Err: errors.New("refresh unavailable")}
	close(h.updates)
	h.wait(t, "Warning: model catalog refresh failed; retaining cached catalog: refresh unavailable")
	h.command("/model")
	h.wait(t, "Family B")
	if !reflect.DeepEqual(h.runtime.ModelChoice(), initial) {
		t.Fatal("failure changed active selection")
	}
	h.key(tcell.KeyDown)
	h.key(tcell.KeyEnter)
	h.key(tcell.KeyEnter)
	h.wait(t, "Model selected for next tool boundary · family-b · high")
	if h.runtime.ModelChoice().Model.Name != "Family B" {
		t.Fatal("failed update replaced cached metadata")
	}
}

func TestClosedCatalogUpdateChannelDoesNotSpinAndCancels(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	close(h.updates)
	h.command("/model")
	h.wait(t, "Model family")
	// The normal redraw ticker runs four times per second. A closed receive
	// left in select would continuously redraw, overwhelming this generous bound.
	before := h.screen.shows.Load()
	time.Sleep(300 * time.Millisecond)
	if delta := h.screen.shows.Load() - before; delta > 20 {
		t.Fatalf("closed catalog channel caused redraw spin: %d frames", delta)
	}
	h.cancel()
	select {
	case err := <-h.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation returned %v", err)
		}
		// Cleanup owns the join, so restore its completed result.
		h.done <- err
	case <-time.After(3 * time.Second):
		t.Fatal("closed channel prevented cancellation")
	}
}
