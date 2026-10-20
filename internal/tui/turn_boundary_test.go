package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/render"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

func TestTurnBoundaryLiveReplay(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	catalog, err := skills.Discover(ctx, w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	var events []session.Event
	var mu sync.Mutex
	// Session naming consumes one additional scripted response.
	provider := &llm.Script{Responses: []llm.ScriptResponse{{Text: "Answer"}, {Text: "Answer"}, {Text: "Answer"}}}
	r, err := session.New(ctx, store, w, provider, llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}, "", catalog, tool.WebSearchConfig{}, func(e session.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, source := range []string{"normal", "queue"} {
		if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: source, InputSource: source, InputTimeMS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	c := &frontendController{frontend: &Frontend{Runtime: r, Output: io.Discard}, ctx: ctx, history: store, view: newTranscript(), sidebar: newSidebar()}
	c.replay()
	mu.Lock()
	savedEvents := append([]session.Event(nil), events...)
	mu.Unlock()
	count := 0
	for _, item := range c.view.lines {
		if !item.human {
			continue
		}
		count++
		var event session.Event
		for _, e := range savedEvents {
			if e.EntryID == item.id && e.Kind == "message" {
				event = e
				break
			}
		}
		live := messageLine(line{}, llm.Message{Type: event.MessageType, Content: event.Text, InputSource: event.InputSource, InputTimeMS: event.InputTimeMS})
		if item.turnTime != live.turnTime {
			t.Fatalf("replay/live boundary mismatch: %+v / %+v", item, live)
		}
		if strings.Contains(item.text, "normal") && item.turnTime == "" {
			t.Fatal("replay lost idle-input divider")
		}
		if strings.Contains(item.text, "queue") && item.turnTime != "" {
			t.Fatal("replay marked queued input")
		}
	}
	if count != 2 {
		t.Fatalf("replayed %d human inputs, want 2", count)
	}
}

func TestTurnBoundaryProjection(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 7, 0, 0, time.Local)
	for _, source := range []string{"", "normal", "queue", "steer", "task", "btw"} {
		t.Run(source, func(t *testing.T) {
			item := messageLine(line{}, llm.Message{Type: llm.TypeUserInput, Content: "prompt", InputSource: source, InputTimeMS: at.UnixMilli()})
			want := ""
			if source == "" || source == "normal" {
				want = "09:07"
			}
			if item.turnTime != want {
				t.Fatalf("turnTime = %q, want %q", item.turnTime, want)
			}
		})
	}
	item := messageLine(line{turnTime: "stale"}, llm.Message{Type: llm.TypeUserInput, Content: "legacy without time"})
	if item.turnTime != "" {
		t.Fatalf("invented timestamp: %+v", item)
	}
}

func TestTurnBoundaryLayout(t *testing.T) {
	for _, width := range []int{1, 8, 40, 80} {
		view := newTranscript()
		view.append(line{text: strings.Repeat("prompt ", 6000), human: true, turnTime: "09:07", id: 1})
		view.append(line{text: "user · in-turn", human: true, id: 2})
		view.resize(width)
		dividers, spacing := 0, 0
		for i := range view.blocks {
			view.measure(i)
			for _, row := range view.blocks[i].rows {
				if ansi.StringWidth(row.text) > width {
					t.Fatalf("row exceeds width %d: %q", width, row.text)
				}
				if row.boundary {
					if view.blocks[i].owner != 0 {
						t.Fatal("in-turn input has boundary")
					}
					if row.text == "" {
						spacing++
					} else {
						dividers++
					}
				}
			}
		}
		if dividers != 1 || spacing != 2 {
			t.Fatalf("width %d: dividers=%d spacing=%d", width, dividers, spacing)
		}
	}
}

func TestTurnBoundaryScreen(t *testing.T) {
	s := newTestScreen(t)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(40, 14)
	view := newTranscript()
	view.append(line{text: "user · prompt", human: true, turnTime: "09:07", id: 1})
	view.append(line{text: "user · steer", human: true, id: 2})
	if err := draw(s, view, newSidebar(), false, nil, nil, -1, newComposer(""), 0, nil, nil, "", nil, llm.Selection{}); err != nil {
		t.Fatal(err)
	}
	for _, y := range []int{0, 1, 3} {
		_, style, _ := s.Get(0, y)
		if style.GetBackground() == tcell.GetColor(render.HumanColor) {
			t.Fatalf("boundary row %d highlighted", y)
		}
	}
	for _, y := range []int{2, 4} {
		_, style, _ := s.Get(0, y)
		if style.GetBackground() != tcell.GetColor(render.HumanColor) {
			t.Fatalf("input row %d not highlighted", y)
		}
	}
	var divider strings.Builder
	for x := range 39 {
		text, _, _ := s.Get(x, 1)
		divider.WriteString(text)
	}
	if !strings.Contains(divider.String(), "You · 09:07") {
		t.Fatal(divider.String())
	}
}
