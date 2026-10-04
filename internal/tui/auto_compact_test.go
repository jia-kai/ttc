package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/provider"
)

type compactUIProvider struct {
	provider.Script
	ready, release chan struct{}
}

func (p *compactUIProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if req.NoTools {
		close(p.ready)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Script.Stream(ctx, req, emit)
}

func TestAutomaticCompactionRefreshesConversationWithoutPopupAndPreservesDraft(t *testing.T) {
	p := &compactUIProvider{Script: provider.Script{Responses: []provider.ScriptResponse{
		{Text: "Initial task complete."},
		{Text: "## Compact checkpoint\n\nOlder notes summarized."},
		{Text: "Continued after automatic compaction."},
	}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("initial task")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	before := u.runtime.Current()
	for _, message := range []provider.Message{
		{Role: "user", Content: "Older research task"},
		{Role: "assistant", Content: strings.Repeat("older evidence ", 6000)},
	} {
		if _, err := u.runtime.Store.Append(before, "", "main", "message", message.Role, true, message); err != nil {
			t.Fatal(err)
		}
	}
	u.typeText("continue")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic summary did not start")
	}
	u.typeText("pending draft")
	u.wait(t, "> pending draft")
	close(p.release)
	frame := u.wait(t, "Continued after automatic compaction.")
	if strings.Contains(frame, "Command result") || strings.Contains(frame, "older evidence") || !strings.Contains(frame, "> pending draft") {
		t.Fatal("compaction failed to replace history or preserve the draft", frame)
	}
	if strings.Count(frame, "Continued after automatic compaction.") != 1 {
		t.Fatal("continuation assistant rendered twice", frame)
	}
	u.wait(t, "Turn complete")
	if u.runtime.Current() == before {
		t.Fatal("UI did not use continuation")
	}
}
