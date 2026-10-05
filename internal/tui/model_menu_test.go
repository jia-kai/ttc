package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/tool"

	"ttc/internal/history"
	"ttc/internal/provider"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/workspace"

	"github.com/gdamore/tcell/v2"
)

func menuModels() []provider.ModelSpec {
	a, b := provider.ScriptModel(), provider.ScriptModel()
	a.ID, a.Name = "family-a", "Family A"
	a.Variants, a.DefaultVariant = []string{"low", "high"}, "low"
	b.ID, b.Name = "family-b", "Family B"
	b.Variants, b.DefaultVariant = []string{"low", "high", "max"}, "high"
	b.VariantDescriptions = map[string]string{"high": "Deeper reasoning"}
	return []provider.ModelSpec{a, b}
}

func TestModelMenuChoicesAndCancellation(t *testing.T) {
	models := menuModels()
	current := provider.Selection{Provider: "script", Model: models[0], Variant: "high"}
	m := newModelMenu(models, current)
	key := func(k tcell.Key) (string, string, bool) {
		return m.key(tcell.NewEventKey(k, 0, 0), 20)
	}
	if !strings.Contains(m.Window.Text, "> Family A") {
		t.Fatal("current family is not focused")
	}
	key(tcell.KeyEnter)
	if !strings.Contains(m.Window.Text, "> high (selected)") {
		t.Fatal("current variant is not focused", m.Window.Text)
	}
	if _, _, closed := key(tcell.KeyEscape); closed || m.variants {
		t.Fatal("Escape must return to model families")
	}
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	if !strings.Contains(m.Window.Text, "> high · Deeper reasoning (default)") {
		t.Fatal("new family must focus its provider default", m.Window.Text)
	}
	key(tcell.KeyDown)
	if id, variant, closed := key(tcell.KeyEnter); !closed || id != "family-b" || variant != "max" {
		t.Fatal(id, variant, closed)
	}
	m = newModelMenu(models, current)
	if id, _, closed := key(tcell.KeyEscape); !closed || id != "" {
		t.Fatal("Escape from families must cancel without selecting")
	}
}

func TestModelMenuNarrowViewportKeepsSelectionVisible(t *testing.T) {
	models := menuModels()
	m := newModelMenu(models, provider.Selection{Model: models[0], Variant: "low"})
	m.key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), "> Family B") {
		t.Fatal("wrapped family cursor is outside viewport", lines)
	}
	m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 20)
	m.key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), "> max") {
		t.Fatal("variant cursor is outside viewport", lines)
	}
	m.key(tcell.NewEventKey(tcell.KeyHome, 0, 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), "> low") {
		t.Fatal("scroll did not return to first variant", lines)
	}
}

type gatedModelProvider struct {
	requests chan provider.Request
	release  chan struct{}
	step     int // Stream calls are serial in this fixture, matching Runtime ownership.
}

func (p *gatedModelProvider) Models(context.Context) ([]provider.ModelSpec, error) {
	return menuModels(), nil
}
func (p *gatedModelProvider) Login(context.Context, provider.LoginUI) error { return nil }
func (p *gatedModelProvider) EstimateReplay(m provider.Message) int {
	return provider.ReplayTokens(m.State)
}
func (p *gatedModelProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	p.requests <- req
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	if p.step == 0 {
		call := provider.ToolCall{ID: "timers", Name: "wakeup_list", Arguments: []byte(`{}`)}
		if err := emit(provider.StreamEvent{Kind: "call", Call: &call}); err != nil {
			return err
		}
	} else if err := emit(provider.StreamEvent{Kind: "text", Text: "Finished turn."}); err != nil {
		return err
	}
	p.step++
	return emit(provider.StreamEvent{Kind: "completed"})
}

func TestModelMenuEntryPointsPreserveActiveTurnAndDraft(t *testing.T) {
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

	models := menuModels()
	selection := provider.Selection{Provider: "script", Model: models[0], Variant: "low"}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 64)
	p := &gatedModelProvider{requests: make(chan provider.Request, 4), release: make(chan struct{})}
	r, err := session.New(ctx, store, w, p, selection, "", catalog, tool.WebSearchConfig{}, func(e session.Event) {
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
	defer func() { cancel(); r.Close() }()
	screen := &observedScreen{SimulationScreen: tcell.NewSimulationScreen("UTF-8"), frames: make(chan string, 64)}
	f := Frontend{Runtime: r, Events: events, Screen: screen, Output: io.Discard, Models: models}
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
	waitFrame := func(needle string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		latest := ""
		for {
			select {
			case latest = <-screen.frames:
				if strings.Contains(latest, needle) {
					return
				}
			case <-deadline:
				t.Fatalf("screen missing %q: %s", needle, latest)
			}
		}
	}
	typeText := func(text string) {
		for _, ch := range text {
			screen.InjectKey(tcell.KeyRune, ch, 0)
		}
	}
	key := func(k tcell.Key) { screen.InjectKey(k, 0, 0) }
	request := func() provider.Request {
		t.Helper()
		select {
		case req := <-p.requests:
			return req
		case <-time.After(3 * time.Second):
			t.Fatal("model request did not arrive")
			return provider.Request{}
		}
	}
	waitFrame("/help")
	screen.SetSize(100, 30)
	screen.PostEventWait(tcell.NewEventResize(100, 30))
	typeText("/model")
	key(tcell.KeyEnter)
	waitFrame("Model family")
	key(tcell.KeyEnter)
	waitFrame("Reasoning variant")
	key(tcell.KeyEscape)
	waitFrame("Model family")
	key(tcell.KeyEscape)
	typeText("first")
	key(tcell.KeyEnter)
	first := request()
	if first.Selection.Model.ID != "family-a" || first.Selection.Variant != "low" {
		t.Fatal(first.Selection)
	}
	typeText("draft stays")
	key(tcell.KeyCtrlX)
	typeText("m")
	waitFrame("Model family")
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	waitFrame("Reasoning variant · Family B")
	key(tcell.KeyEnd)
	key(tcell.KeyEnter)
	waitFrame("Model selected for next tool boundary · family-b · max")
	waitFrame("> draft stays")
	p.release <- struct{}{}
	second := request()
	if second.Selection.Model.ID != "family-b" || second.Selection.Variant != "max" {
		t.Fatal("next tool boundary did not switch model", second.Selection)
	}
	p.release <- struct{}{}
	waitFrame("Turn complete")
	key(tcell.KeyEnter)
	third := request()
	if third.Selection.Model.ID != "family-b" || third.Selection.Variant != "max" || third.Messages[len(third.Messages)-1].Content != "draft stays" {
		t.Fatal("next turn did not use chosen model and preserved draft", third.Selection, third.Messages)
	}
}
