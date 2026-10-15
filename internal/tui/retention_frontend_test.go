package tui

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

func TestStartupHistoryStatsVisibleInTUIWithoutInferenceOrPersistence(t *testing.T) {
	p := &helpTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{{Text: "First answer."}}}, requests: make(chan llm.Request, 8), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	frame := u.wait(t, "History database:")
	if !strings.Contains(frame, "0 entries") || !strings.Contains(frame, "oldest n/a") {
		t.Fatal("history pane omitted storage details", frame)
	}
	select {
	case <-p.requests:
		t.Fatal("startup storage status invoked inference")
	default:
	}
	for _, table := range []string{"sessions", "entries", "model_requests"} {
		var count int
		if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("startup status persisted %s: count=%d err=%v", table, count, err)
		}
	}
	// The UI-only message must not enter the first real model request either.
	u.typeText("first prompt")
	u.key(tcell.KeyEnter)
	select {
	case request := <-p.requests:
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "History database:") {
				t.Fatal("startup storage status entered model input")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first requested inference did not start")
	}
	close(p.release)
}

func TestStartupHistoryStatsPrequeuedBeforePlainReplay(t *testing.T) {
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	events := make(chan session.Event, 8)
	r, err := session.New(ctx, store, w, &llm.Script{}, selection, "", catalog, tool.WebSearchConfig{}, func(event session.Event) {
		select {
		case events <- event:
		case <-ctx.Done():
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	select {
	case event := <-events:
		if !strings.HasPrefix(event.Text, "History database:") {
			t.Fatal("missing startup stats", event)
		}
		events <- event // Exercise an event emitted before initial history replay.
	case <-time.After(3 * time.Second):
		t.Fatal("startup stats were not emitted")
	}
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	output := make(chan string, 32)
	frontend := Frontend{Runtime: r, Events: events, Input: input, Output: eventWriter{output}, Plain: true}
	done := make(chan error, 1)
	go func() { done <- frontend.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("plain frontend did not stop")
		}
	}()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case text := <-output:
			if strings.Contains(text, "History database:") {
				if !strings.Contains(text, "0 entries · oldest n/a (empty history)") {
					t.Fatal("plain history output omitted storage details", text)
				}
				return
			}
		case <-deadline:
			t.Fatal("initial replay lost prequeued startup storage status")
		}
	}
}
