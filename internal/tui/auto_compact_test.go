package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

type compactUIProvider struct {
	llm.Script
	ready, release chan struct{}
	mu             sync.Mutex
	initial        llm.Request
}

func (p *compactUIProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	p.mu.Lock()
	if p.initial.System == "" {
		p.initial = req
	}
	p.mu.Unlock()
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

// compactionFixtureEvidence crosses the coding trigger while leaving half the
// coding output allowance for changes between the seed and summary requests.
// The summary must still fit with the original system/tools and final directive.
func compactionFixtureEvidence(t *testing.T, req llm.Request) string {
	t.Helper()
	b := req.Selection.Model.Budget
	input := contextbuild.EstimateInput(contextbuild.InputSnapshot{Selection: req.Selection, System: req.System, Tools: req.Tools, Messages: req.Messages}).Input
	directive := contextbuild.Tokens([]llm.Message{{Type: llm.TypeRuntimeInstruction, Content: prompts.Compaction}})
	tokens := b.ContextLimit - input - b.OutputAllowance - b.EstimationMargin - b.SummaryOutputAllowance - directive + b.OutputAllowance/2
	if input < 0 || tokens <= b.RecentTokensMax {
		t.Fatalf("fixture has insufficient room for compactable evidence: input=%d evidence=%d budget=%+v", input, tokens, b)
	}
	const phrase = "older evidence "
	return strings.Repeat(phrase, tokens/contextbuild.Estimate(phrase))
}

func TestAutomaticCompactionRefreshesConversationWithoutPopupAndPreservesDraft(t *testing.T) {
	p := &compactUIProvider{Script: llm.Script{Responses: []llm.ScriptResponse{
		{Text: "Initial task complete."},
		{Text: "## Compact checkpoint\n\nOlder notes summarized."},
		{Text: "Continued after automatic compaction."},
	}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("initial task")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	before := u.runtime.Current()
	p.mu.Lock()
	initial := p.initial
	p.mu.Unlock()
	for _, message := range []llm.Message{
		{Type: llm.TypeUserInput, Content: "Older research task"},
		{Type: llm.TypeAssistant, Content: compactionFixtureEvidence(t, initial)},
	} {
		if _, err := u.runtime.Store.Append(before, "", "main", "message", message.Role(), true, message); err != nil {
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
