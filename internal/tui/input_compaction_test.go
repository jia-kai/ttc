package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
	"scicode/internal/session"
)

// inputCompactionProvider gates the real summarizer and first continuation
// request independently, so input can be inspected before and after handoff.
type inputCompactionProvider struct {
	provider.Script
	mu                            sync.Mutex
	selection                     provider.Selection
	requests                      []provider.Request
	summaries                     int
	summarizing, finishSummary    chan struct{}
	continued, finishContinuation chan struct{}
}

func (p *inputCompactionProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	p.mu.Lock()
	if req.NoTools {
		p.summaries++
		count := p.summaries
		p.mu.Unlock()
		if count != 1 {
			return fmt.Errorf("unexpected repeated automatic compaction")
		}
		close(p.summarizing)
		select {
		case <-p.finishSummary:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Research handoff completed."})
	}
	p.requests = append(p.requests, req)
	step := len(p.requests) - 1 // The initial request seeds old history before the UI starts.
	if step == 0 {
		selection := req.Selection
		selection.Model.ID = "input-compaction-small-context"
		base := contextbuild.Estimate(req.System) + selection.Model.Budget.OutputAllowance + selection.Model.Budget.EstimationMargin
		for _, tool := range req.Tools {
			base += contextbuild.Estimate(tool.Description) + contextbuild.Estimate(string(tool.Parameters)) + 16
		}
		for _, message := range req.Messages {
			if message.Role != "user" || message.Runtime {
				base += contextbuild.Tokens([]provider.Message{message})
			}
		}
		selection.Model.Budget.ContextLimit = base + 3000
		selection.Model.Budget.RecentTokensMin = 0
		selection.Model.Budget.RecentTokensMax = 700
		selection.Model.Budget.NextTurnInputReserve = 512
		selection.Model.Budget.SummaryOutputAllowance = 512
		p.selection = selection
	}
	p.mu.Unlock()
	if step == 1 {
		close(p.continued)
		select {
		case <-p.finishContinuation:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if step > 3 {
		return fmt.Errorf("unexpected extra coding request %d", step)
	}
	if step == 0 {
		return emit(provider.StreamEvent{Kind: "text", Text: strings.Repeat("Earlier research notes. ", 700)})
	}
	return emit(provider.StreamEvent{Kind: "text", Text: fmt.Sprintf("Input compaction response %d.", step)})
}

func TestAutomaticCompactionRetainsQueuedAndSteeredAttachmentSnapshots(t *testing.T) {
	p := &inputCompactionProvider{
		summarizing: make(chan struct{}), finishSummary: make(chan struct{}),
		continued: make(chan struct{}), finishContinuation: make(chan struct{}),
	}
	u := newQuestionTestUIWithSetup(t, p, nil, func(r *session.Runtime) {
		// Completed model output fills context. Both earlier human prompts and
		// their small snapshots must now survive independently of the cycle cap.
		seed := (contextbuild.Input{Text: "Earlier task", Attachments: []contextbuild.Attachment{{Path: "prior-notes.txt", Kind: "text", Text: "Earlier snapshot."}}}).Message()
		if err := r.Run(&seed); err != nil {
			t.Fatal(err)
		}
		if err := r.RequestModel(p.selection); err != nil {
			t.Fatal(err)
		}
	})
	before, generation := u.runtime.Current(), u.runtime.Generation()
	path := filepath.Join(u.runtime.Workspace.Root, "pending.txt")
	const original = "ORIGINAL PENDING INPUT SNAPSHOT"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := contextbuild.Snapshot(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"Queued first", "Queued second", "Steered first", "Steered second"}
	u.typeText("Continue current work")
	u.key(tcell.KeyEnter)
	waitInputCompaction(t, p.summarizing, "automatic summarization")
	for i, text := range texts {
		u.typeText("/attach " + path)
		u.key(tcell.KeyEnter)
		u.wait(t, "Attached")
		u.typeText(text)
		if i < 2 {
			u.key(tcell.KeyEnter)
			u.wait(t, "Queued · "+text)
		} else {
			u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
			u.wait(t, "Steer · "+text)
		}
	}
	if err := os.WriteFile(path, []byte("CHANGED AFTER SUBMISSION"), 0600); err != nil {
		t.Fatal(err)
	}
	close(p.finishSummary)
	waitInputCompaction(t, p.continued, "coding after automatic handoff")
	if u.runtime.Current() == before || u.runtime.Generation() != generation {
		t.Fatal("automatic handoff did not preserve live ownership", u.runtime.Current(), u.runtime.Generation())
	}
	// Cancellation still returns the original composer and attachment snapshot
	// after continuation replay. Resubmitting must neither reread nor duplicate it.
	u.typeText("/cancel-queue")
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "Cancelled pending input · restored to composer")
	if !strings.Contains(frame, "Queued second") {
		t.Fatal("post-handoff cancellation did not restore the composer", frame)
	}
	var queuedEntries int
	if err := u.runtime.Store.DB.QueryRow(`SELECT count(*) FROM entries WHERE model_visible=1 AND role='user' AND json_extract(content_json,'$.content') LIKE 'Queued %'`).Scan(&queuedEntries); err != nil || queuedEntries != 0 {
		t.Fatal("queued or cancelled input entered history before admission", queuedEntries, err)
	}
	u.key(tcell.KeyEnter)
	u.wait(t, "Queued · Queued second")
	close(p.finishContinuation)
	u.wait(t, "Input compaction response 3.")
	u.wait(t, "Turn complete")
	p.mu.Lock()
	requests := append([]provider.Request(nil), p.requests...)
	summaries := p.summaries
	p.mu.Unlock()
	if len(requests) != 4 || summaries != 1 {
		t.Fatal("pending inputs lost or replayed", len(requests), summaries)
	}
	committedTimes := make(map[string]int64)
	want := func(text string, got provider.Message) provider.Message {
		source := "queue"
		if strings.HasPrefix(text, "Steered ") {
			source = "steer"
		}
		if got.InputSource != source || got.InputTimeMS <= 0 {
			t.Fatalf("wrong admitted provenance for %q: %#v", text, got)
		}
		if at, ok := committedTimes[text]; ok && at != got.InputTimeMS {
			t.Fatalf("commit time changed for %q: %d -> %d", text, at, got.InputTimeMS)
		}
		committedTimes[text] = got.InputTimeMS
		message := (contextbuild.Input{Text: text, Source: source, Attachments: []contextbuild.Attachment{snapshot}}).Message()
		message.InputTimeMS = got.InputTimeMS
		return message
	}
	for i, request := range requests[1:] {
		var found []provider.Message
		for _, message := range request.Messages {
			if message.Role == "user" && !message.Runtime && (strings.HasPrefix(message.Content, "Steered ") || strings.HasPrefix(message.Content, "Queued ")) {
				found = append(found, message)
			}
		}
		expectedTexts := []string{texts[2], texts[3]}
		if i >= 1 {
			expectedTexts = append(expectedTexts, texts[0])
		}
		if i >= 2 {
			expectedTexts = append(expectedTexts, texts[1])
		}
		if len(found) != len(expectedTexts) {
			t.Fatalf("request %d: got %d pending inputs, want %d", i+1, len(found), len(expectedTexts))
		}
		var expected []provider.Message
		for j, text := range expectedTexts {
			expected = append(expected, want(text, found[j]))
		}
		if !reflect.DeepEqual(found, expected) {
			t.Fatalf("request %d lost FIFO ordering, exactly-once delivery or immutable attachments: got %#v, want %#v", i+1, found, expected)
		}
	}
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	var humans []provider.Message
	for _, message := range messages {
		if message.Role == "user" && !message.Runtime && (strings.HasPrefix(message.Content, "Steered ") || strings.HasPrefix(message.Content, "Queued ")) {
			humans = append(humans, message)
		}
	}
	if len(humans) != 4 {
		t.Fatal("wrong persisted pending input count", humans)
	}
	expected := []provider.Message{want(texts[2], humans[0]), want(texts[3], humans[1]), want(texts[0], humans[2]), want(texts[1], humans[3])}
	if !reflect.DeepEqual(humans, expected) {
		t.Fatal("pending inputs were not persisted exactly once", humans)
	}
	var steerCheckpoints, userTurns int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='steer'").Scan(&steerCheckpoints); err != nil || steerCheckpoints != 2 {
		t.Fatal("wrong steering checkpoint count", steerCheckpoints, err)
	}
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='user'").Scan(&userTurns); err != nil || userTurns != 4 {
		t.Fatal("queue cancellation created a turn or queued input was not admitted once", userTurns, err)
	}
}

func waitInputCompaction(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
