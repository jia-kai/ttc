package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

type scrollProvider struct {
	provider.Script
	release chan struct{}
}

func (p *scrollProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	var text strings.Builder
	for n := range 50 {
		fmt.Fprintf(&text, "line-%02d\n", n)
	}
	if err := emit(provider.StreamEvent{Kind: "text", Text: text.String()}); err != nil {
		return err
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return emit(provider.StreamEvent{Kind: "text", Text: "New streaming tail"})
}

func TestCtrlDAtBottomFollowsDuringActiveTurn(t *testing.T) {
	p := &scrollProvider{release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("stream")
	u.key(tcell.KeyEnter)
	u.wait(t, "line-49")
	u.key(tcell.KeyCtrlU)
	u.wait(t, "line-19")
	for range 30 {
		u.key(tcell.KeyCtrlD)
	}
	u.wait(t, "line-49")
	u.key(tcell.KeyCtrlD)
	u.typeText("retained draft")
	u.wait(t, "> retained draft") // Ensure the Ctrl+D key was consumed before new text.
	close(p.release)
	u.wait(t, "New streaming tail")
}
