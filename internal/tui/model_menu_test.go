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
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/workspace"

	"github.com/gdamore/tcell/v3"
)

func menuModels() []llm.ModelSpec {
	a, b := llm.ScriptModel(), llm.ScriptModel()
	a.ID, a.Name = "family-a", "Family A"
	a.Variants, a.DefaultVariant = []string{"low", "high"}, "low"
	b.ID, b.Name = "family-b", "Family B"
	b.Variants, b.DefaultVariant = []string{"low", "high", "max"}, "high"
	b.VariantDescriptions = map[string]string{"high": "Deeper reasoning"}
	return []llm.ModelSpec{a, b}
}

func TestModelMenuMarkers(t *testing.T) {
	models := menuModels()
	current := llm.Selection{Model: models[0], Variant: "low"}
	m := newModelMenu(models, current)
	easy := llm.Selection{Model: models[1], Variant: "high"}
	m.easy = &easy
	m.update()
	if m.rows[0] != ">*  Family A · family-a" || m.rows[1] != " E  Family B · family-b" {
		t.Fatalf("family markers: %q", m.rows)
	}
	m.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 20)
	if m.rows[0] != ">*  low (default)" || m.rows[1] != "    high" {
		t.Fatalf("variant markers: %q", m.rows)
	}
	m.easy = &current
	m.update()
	if m.rows[0] != ">E* low (default)" {
		t.Fatalf("combined markers: %q", m.rows[0])
	}
}

func TestModelMenuChoicesAndCancellation(t *testing.T) {
	models := menuModels()
	current := llm.Selection{Provider: "script", Model: models[0], Variant: "high"}
	m := newModelMenu(models, current)
	key := func(k tcell.Key) (string, string, bool) {
		return m.key(tcell.NewEventKey(k, "", 0), 20)
	}
	if !strings.Contains(m.Window.Text, ">*  Family A") {
		t.Fatal("current family is not focused")
	}
	m.key(tcell.NewEventKey(tcell.KeyRune, "E", 0), 20)
	if !m.variants || !strings.Contains(m.Window.Hint, "E toggles easy") {
		t.Fatal("E on family must open variants with an easy-toggle hint")
	}
	if !strings.Contains(m.Window.Text, ">*  high") {
		t.Fatal("current variant is not focused", m.Window.Text)
	}
	if _, _, closed := key(tcell.KeyEscape); closed || m.variants {
		t.Fatal("Escape must return to model families")
	}
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	if !strings.Contains(m.Window.Text, ">   high · Deeper reasoning (default)") {
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
	m := newModelMenu(models, llm.Selection{Model: models[0], Variant: "low"})
	m.key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), ">   Family B") {
		t.Fatal("wrapped family cursor is outside viewport", lines)
	}
	m.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 20)
	m.key(tcell.NewEventKey(tcell.KeyEnd, "", 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), ">   max") {
		t.Fatal("variant cursor is outside viewport", lines)
	}
	m.key(tcell.NewEventKey(tcell.KeyHome, "", 0), 20)
	m.reveal(16, 2)
	if lines := m.Window.Lines(16, 2); !strings.Contains(strings.Join(lines, "\n"), ">   low") {
		t.Fatal("scroll did not return to first variant", lines)
	}
}

func TestLegacyEasyModelCommandRejected(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	h.command("/easy-model family-b high")
	h.wait(t, "unknown command /easy-model")
	if easy := h.runtime.EasyModelChoice(); easy != nil {
		t.Fatalf("legacy command changed easy selection: %+v", easy)
	}
}

func TestModelStatusShowsEasyChoice(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	text := h.wait(t, "family-a · low")
	if strings.Contains(text, "Ctrl+X F full") || strings.Contains(text, "Ctrl+X M model") || strings.Contains(text, "low · /help") {
		t.Fatal("status contains shortcut hints", text)
	}
	h.command("/model easy family-b high")
	h.wait(t, "family-a · low [family-b · high]")
	h.command("/model easy off")
	text = h.wait(t, "Easy model disabled")
	if !strings.Contains(text, "family-a · low") || strings.Contains(text, "[family-b · high]") {
		t.Fatal("status retained cleared easy choice", text)
	}
}

func TestEasyChoiceWhileViewingOtherProviderArchive(t *testing.T) {
	h := newCatalogUpdateFixture(t)
	r := h.runtime
	archive := llm.Selection{Provider: "archived", Model: menuModels()[0], Variant: "low"}
	if err := r.Store.SaveEasySelection(archive.Provider, &archive); err != nil {
		t.Fatal(err)
	}
	id := history.NewID("session")
	turn, _, err := r.Store.StartSession(id, r.Workspace.Root, archive, llm.Message{Type: llm.TypeUserInput, Content: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET read_only=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if err := r.Load(id); err != nil || r.CurrentSelection().Provider != "archived" {
		t.Fatal("archive load", err, r.CurrentSelection())
	}
	h.command("/model easy family-b high")
	h.wait(t, "Easy model selected · family-b · high")
	got, err := r.Store.LastEasySelection(r.ProviderID())
	if err != nil || got == nil || got.Provider != r.ProviderID() || got.Model.ID != "family-b" {
		t.Fatal("backing preference", got, err)
	}
	got, err = r.Store.LastEasySelection("archived")
	if err != nil || got == nil || got.Model.ID != archive.Model.ID {
		t.Fatal("archive preference changed", got, err)
	}
	h.command("/model easy off")
	h.wait(t, "Easy model disabled")
	got, err = r.Store.LastEasySelection(r.ProviderID())
	if err != nil || got != nil {
		t.Fatal("backing preference not cleared", got, err)
	}
	got, err = r.Store.LastEasySelection("archived")
	if err != nil || got == nil {
		t.Fatal("archive preference cleared", got, err)
	}
}

func TestModelCommandsAddressReservedIDs(t *testing.T) {
	models := menuModels()
	models[0].ID = "easy"
	models[1].ID = "off"
	h := newCatalogUpdateFixtureModels(t, models)
	h.command("/model main easy high")
	h.wait(t, "Model switched · easy · high")
	if got := h.runtime.ModelChoice(); got.Model.ID != "easy" || got.Variant != "high" {
		t.Fatal(got)
	}
	h.command("/model easy -- off low")
	h.wait(t, "Easy model selected · off · low")
	if got := h.runtime.EasyModelChoice(); got == nil || got.Model.ID != "off" || got.Variant != "low" {
		t.Fatal(got)
	}
	h.command("/model easy off")
	h.wait(t, "Easy model disabled")
	if h.runtime.EasyModelChoice() != nil {
		t.Fatal("clear did not clear")
	}
}

func TestModelMenuNarrowRendersEasyHintAndMarker(t *testing.T) {
	models := menuModels()
	models[0].Name = strings.Repeat("long-name", 8)
	models[0].Description = strings.Repeat("long description ", 8)
	models[0].VariantDescriptions = map[string]string{"low": strings.Repeat("long description ", 8)}
	choice := llm.Selection{Provider: "script", Model: models[0], Variant: "low"}
	m := newModelMenu(models, choice)
	m.easy = &choice
	m.update()
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(40, 20)
	drawWindow(s, &m.Window)
	if text := screenText(s); !strings.Contains(text, "E/Enter variants") || !strings.Contains(text, ">E* ") {
		t.Fatal("narrow family view hides easy action or marker", text)
	}
	m.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 10)
	drawWindow(s, &m.Window)
	if text := screenText(s); !strings.Contains(text, "E toggles easy") || !strings.Contains(text, ">E* low") {
		t.Fatal("narrow variant view hides easy action or marker", text)
	}
}

type gatedModelProvider struct {
	llm.Script
	requests chan llm.Request
	release  chan struct{}
	step     int // Stream calls are serial in this fixture, matching Runtime ownership.
}

func (p *gatedModelProvider) EstimateReplay(m llm.Message) int {
	return llm.ReplayTokens(m.State)
}
func (p *gatedModelProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	p.requests <- req
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
	}
	if p.step == 0 {
		call := llm.ToolCall{ID: "timers", Name: "wakeup_list", Arguments: []byte(`{}`)}
		if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
			return err
		}
	} else if err := emit(llm.StreamEvent{Kind: "text", Text: "Finished turn."}); err != nil {
		return err
	}
	p.step++
	return emit(llm.StreamEvent{Kind: "completed"})
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
	selection := llm.Selection{Provider: "script", Model: models[0], Variant: "low"}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 64)
	p := &gatedModelProvider{requests: make(chan llm.Request, 4), release: make(chan struct{})}
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
	screen := &observedScreen{Screen: newTestScreen(t), frames: make(chan string, 64)}
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
			screen.EventQ() <- tcell.NewEventKey(tcell.KeyRune, string(ch), 0)
		}
	}
	key := func(k tcell.Key) { screen.EventQ() <- tcell.NewEventKey(k, "", 0) }
	request := func() llm.Request {
		t.Helper()
		select {
		case req := <-p.requests:
			return req
		case <-time.After(3 * time.Second):
			t.Fatal("model request did not arrive")
			return llm.Request{}
		}
	}
	waitFrame("family-a · low")
	screen.SetSize(100, 30)
	screen.EventQ() <- tcell.NewEventResize(100, 30)
	typeText("/model")
	key(tcell.KeyEnter)
	waitFrame("Model family")
	key(tcell.KeyEnter)
	waitFrame("Reasoning variant")
	typeText("e") // The main model can also be the explicit easy model.
	waitFrame(">E* low (default)")
	if easy := r.EasyModelChoice(); easy == nil || easy.Model.ID != "family-a" || easy.Variant != "low" {
		t.Fatal("same main/easy choice not saved", easy)
	}
	key(tcell.KeyEscape)
	waitFrame("Model family")
	waitFrame(">E* Family A · family-a")
	key(tcell.KeyDown)
	key(tcell.KeyEnter)
	waitFrame("Reasoning variant")
	typeText("E")
	waitFrame(">E  high · Deeper reasoning (default)")
	if easy := r.EasyModelChoice(); easy == nil || easy.Model.ID != "family-b" || easy.Variant != "high" {
		t.Fatal("easy choice not switched", easy)
	}
	typeText("e")
	waitFrame("high · Deeper reasoning (default)   ")
	if easy := r.EasyModelChoice(); easy != nil {
		t.Fatal("easy choice not cleared", easy)
	}
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
	waitFrame("Reasoning variant")
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
