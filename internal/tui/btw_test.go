package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

type btwUIProvider struct {
	provider.Script
	mainReady, asideReady, releaseAside chan struct{}
}

func (p *btwUIProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if !strings.Contains(req.ConversationID, "/btw_") {
		close(p.mainReady)
		if err := emit(provider.StreamEvent{Kind: "text", Text: "Main still working"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	close(p.asideReady)
	select {
	case <-p.releaseAside:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := emit(provider.StreamEvent{Kind: "text", Text: "## Popup answer\n\nRead-only evidence."}); err != nil {
		return err
	}
	return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 100, OutputTokens: 10}})
}

func TestBTWPopupWaitsForFullscreenAndPreservesMain(t *testing.T) {
	p := &btwUIProvider{mainReady: make(chan struct{}), asideReady: make(chan struct{}), releaseAside: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("main work")
	u.key(tcell.KeyEnter)
	u.wait(t, "Main still working")
	u.typeText("/btw explain this")
	u.key(tcell.KeyEnter)
	select {
	case <-p.asideReady:
	case <-time.After(3 * time.Second):
		t.Fatal("aside not admitted while main busy")
	}
	u.key(tcell.KeyCtrlX)
	u.typeText("f")
	u.wait(t, "updates paused")
	close(p.releaseAside)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var completions int
		if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='job_completion'").Scan(&completions); err != nil {
			t.Fatal(err)
		}
		if completions > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("aside not completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	u.key(tcell.KeyEsc)
	u.wait(t, "read-only answer")
	u.wait(t, "Popup answer")
	u.runtime.Interrupt()
	u.key(tcell.KeyEsc)
	u.wait(t, "Turn interrupted")
}
