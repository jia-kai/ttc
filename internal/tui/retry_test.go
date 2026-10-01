package tui

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

type retryUIProvider struct{ provider.Script }

func (p *retryUIProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{Attempt: 5, DelayMilliseconds: 1500, Reason: "HTTP 429"}}); err != nil {
		return err
	}
	return emit(provider.StreamEvent{Kind: "text", Text: "Recovered"})
}

func TestRetrySystemMessageOpensInspector(t *testing.T) {
	u := newQuestionTestUI(t, &retryUIProvider{})
	u.typeText("retry")
	u.key(tcell.KeyEnter)
	u.wait(t, "Retrying · attempt 5 in 1.5s · HTTP 429")
	u.wait(t, "Turn completed")
	var id int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='model_retry'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, "model_retry")
	u.wait(t, "delay_ms")
}

func TestComposerCursorTracksWidthAndHidesForWindow(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(20, 10)
	draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, newComposer("λ界"), 0, nil, false, time.Time{}, nil, provider.Selection{})
	x, y, visible := s.GetCursor()
	if !visible || x != 5 || y != 8 {
		t.Fatal(x, y, visible)
	}
	draw(s, newTranscript(), newSidebar(), false, nil, nil, 0, newComposer("λ界"), 0, nil, false, time.Time{}, &Window{Title: "Inspector"}, provider.Selection{})
	_, _, visible = s.GetCursor()
	if visible {
		t.Fatal("composer cursor visible inside viewer")
	}
}
