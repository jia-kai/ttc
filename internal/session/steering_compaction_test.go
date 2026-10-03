package session

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

	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
)

func TestAutomaticCompactionRetainsSteeringSnapshotsAndCancellation(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	compactionBudget(t, r)
	// Leave room for an immutable image as well as pending text instructions.
	r.selection.Model.Budget.ContextLimit += 8192
	seedCompactionHistory(t, r, strings.Repeat("Earlier research notes. ", 1800))
	before, generation := r.Current(), r.Generation()
	path := filepath.Join(r.Workspace.Root, "steering.txt")
	if err := os.WriteFile(path, []byte("ORIGINAL STEERING SNAPSHOT"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := contextbuild.Snapshot(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	image := contextbuild.Attachment{Path: "fixture.png", Kind: "image", Image: &provider.Image{Path: "fixture.png", DataURL: "data:image/png;base64,aW1tdXRhYmxl"}}
	inputs := []contextbuild.Input{
		{Text: "First pending steer", Attachments: []contextbuild.Attachment{snapshot, image}},
		{Text: "Second pending steer", Attachments: []contextbuild.Attachment{snapshot}},
		{Text: "Cancel after handoff", Attachments: []contextbuild.Attachment{snapshot}},
	}
	summarizing, finishSummary := make(chan struct{}), make(chan struct{})
	handedOff, finishHandoff := make(chan struct{}), make(chan struct{})
	var releaseHandoff sync.Once
	// Pause at the actual automatic continuation event, before request admission.
	r.Emit = func(event Event) {
		events <- event
		if event.Kind == "continuation" {
			close(handedOff)
			select {
			case <-finishHandoff:
			case <-r.ctx.Done():
			}
		}
	}
	summaries, coding := 0, 0
	var committed []provider.Message
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if req.NoTools {
			summaries++
			if summaries != 1 {
				return fmt.Errorf("unexpected repeated compaction")
			}
			close(summarizing)
			select {
			case <-finishSummary:
			case <-ctx.Done():
				return ctx.Err()
			}
			return emit(provider.StreamEvent{Kind: "text", Text: "Earlier research is archived."})
		}
		coding++
		if r.Current() == before {
			return fmt.Errorf("coding began before automatic handoff")
		}
		var found []provider.Message
		for _, message := range req.Messages {
			if message.Role == "user" && !message.Runtime && strings.Contains(message.Content, "pending steer") {
				found = append(found, message)
			}
			if message.Role == "user" && !message.Runtime && strings.Contains(message.Content, "Cancel after handoff") {
				return fmt.Errorf("cancelled steer reached inference")
			}
		}
		want := []provider.Message{inputs[0].Message(), inputs[1].Message()}
		if len(found) != len(want) {
			return fmt.Errorf("wrong admitted steering count: %d", len(found))
		}
		for i := range want {
			if found[i].InputTimeMS <= 0 {
				return fmt.Errorf("steer %d has no commit timestamp", i)
			}
			want[i].InputSource = "steer"
			want[i].InputTimeMS = found[i].InputTimeMS
		}
		if !reflect.DeepEqual(found, want) {
			return fmt.Errorf("steering order, multiplicity or snapshots changed: got %#v, want %#v", found, want)
		}
		if coding == 1 {
			committed = found
		} else if !reflect.DeepEqual(found, committed) {
			return fmt.Errorf("admitted steering provenance changed across requests")
		}
		if coding == 1 {
			return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "read-after-handoff", Name: "read", Arguments: []byte(`{"path":"steering.txt"}`)}})
		}
		if coding != 2 {
			return fmt.Errorf("unexpected coding request %d", coding)
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Steering completed."})
	}}
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		done <- r.Run(&provider.Message{Role: "user", Content: "Continue current work"})
		close(stopped)
	}()
	t.Cleanup(func() {
		r.Interrupt()
		releaseHandoff.Do(func() { close(finishHandoff) })
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("compaction run did not stop")
		}
	})
	select {
	case <-summarizing:
	case err := <-done:
		t.Fatalf("run ended before automatic summarization: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for automatic summarization")
	}
	for _, input := range inputs {
		if err := r.Steer(input); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("CHANGED ON DISK"), 0600); err != nil {
		t.Fatal(err)
	}
	close(finishSummary)
	waitCompactionSignal(t, handedOff, "automatic handoff")
	if r.Current() == before || r.Generation() != generation {
		t.Fatal("handoff did not preserve live ownership", r.Current(), r.Generation())
	}
	r.orderMu.Lock()
	pending := append([]contextbuild.Input(nil), r.steers...)
	r.orderMu.Unlock()
	if !reflect.DeepEqual(pending, inputs) {
		t.Fatal("handoff changed pending steering snapshots", pending)
	}
	cancelled, err := r.CancelSteer()
	if err != nil || !reflect.DeepEqual(cancelled, inputs[2]) {
		t.Fatal("post-handoff cancellation lost the original input", cancelled, err)
	}
	releaseHandoff.Do(func() { close(finishHandoff) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("automatic continuation did not finish")
	}
	if count, _ := r.SteeringPreview(0); summaries != 1 || coding != 2 || count != 0 {
		t.Fatal("pending inputs were lost or replayed", summaries, coding, count)
	}
	if _, err := r.CancelSteer(); err == nil {
		t.Fatal("already admitted steer remained cancellable")
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	var humans []provider.Message
	for _, message := range messages {
		if message.Role == "user" && !message.Runtime && (strings.Contains(message.Content, "pending steer") || strings.Contains(message.Content, "Cancel after handoff")) {
			humans = append(humans, message)
		}
	}
	if !reflect.DeepEqual(humans, committed) {
		t.Fatal("steering was not persisted exactly once in FIFO order", humans)
	}
	var checkpoints int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE trigger='steer'").Scan(&checkpoints); err != nil || checkpoints != 2 {
		t.Fatal("cancelled steer created a checkpoint or admitted steer duplicated one", checkpoints, err)
	}
}

func waitCompactionSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
