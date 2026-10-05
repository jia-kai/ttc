package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"ttc/internal/provider"
)

func TestHelpSendShortcuts(t *testing.T) {
	for _, want := range []string{
		"Keyboard shortcuts apply to the TUI; plain-mode messages queue while busy.",
		"**Enter / Alt+Enter while idle** — Start a normal turn.",
		"**Enter while busy** — Steer the active main turn",
		"**Alt+Enter while busy** — Queue a new turn FIFO.",
		"**Foreground shells** — Delay steer delivery",
	} {
		if !strings.Contains(helpMarkdown, want) {
			t.Errorf("help is missing %q", want)
		}
	}
}

type helpGatedProvider struct {
	provider.Script
	ready, release chan struct{}
}

func (p *helpGatedProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	close(p.ready)
	select {
	case <-p.release:
		return p.Script.Stream(ctx, req, emit)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestHelpCommandDoesNotWaitForActiveTurn(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	p := &helpGatedProvider{Script: provider.Script{Responses: []provider.ScriptResponse{{Text: "Completed."}}}, ready: make(chan struct{}), release: make(chan struct{})}
	r.Provider = p
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		m := provider.Message{Role: "user", Content: "start"}
		done <- r.Run(&m)
	}()
	t.Cleanup(func() {
		r.Interrupt()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("turn did not stop")
		}
	})
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	current, generation := r.Current(), r.Generation()
	helped := make(chan struct{})
	go func() {
		defer close(helped)
		text, err := r.Command(" \t/help\n")
		if err != nil || text != helpMarkdown {
			t.Errorf("help = %q, %v", text, err)
		}
	}()
	select {
	case <-helped:
	case <-time.After(time.Second):
		t.Fatal("help waited for the active response")
	}
	if r.Current() != current || r.Generation() != generation {
		t.Fatal("help changed the live session")
	}
	select {
	case err := <-done:
		t.Fatal("help stopped the active turn", err)
	default:
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish")
	}
	var requests int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM model_requests WHERE purpose='coding'").Scan(&requests); err != nil || requests != 1 {
		t.Fatal("help made an inference request", requests, err)
	}
}

func TestHelpCommandRequiresExactCommand(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, command := range []string{"/helper", "/help extra"} {
		if _, err := r.Command(command); err == nil {
			t.Fatalf("accepted %q", command)
		}
	}
}
