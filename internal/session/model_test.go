package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/tool"
)

func TestModelSwitchAtToolBoundaryPreservesPerRequestState(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	a := r.CurrentSelection()
	b := a
	b.Model.ID = "other"
	fast := b
	fast.Model.ID = "other/fast"
	fast.Model.BaseID = "other"
	fast.Model.ServiceTier = "priority"
	entered, release := make(chan struct{}, 2), make(chan struct{})
	r.Tools = tool.NewRegistry()
	tool.Register(r.Tools, "shell", "gated tool", map[string]any{}, nil, func(struct{}) error { return nil }, func(ctx context.Context, _ tool.Execution, _ struct{}) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return map[string]any{"status": "completed"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	requests := make(chan provider.Request, 3)
	step := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		requests <- req
		if step == 2 {
			return emit(provider.StreamEvent{Kind: "text", Text: "finished"})
		}
		item := json.RawMessage(fmt.Sprintf(`{"type":"reasoning","encrypted_content":"%s"}`, req.Selection.Model.ID))
		if err := emit(provider.StreamEvent{Kind: "state", StateVersion: 1, StateItem: item}); err != nil {
			return err
		}
		call := provider.ToolCall{ID: fmt.Sprint(step), Name: "shell", Arguments: []byte(`{}`)}
		step++
		return emit(provider.StreamEvent{Kind: "call", Call: &call})
	}}
	done := make(chan error, 1)
	go func() { done <- r.Run(&provider.Message{Role: "user", Content: "switch at tools"}) }()
	waitEntered := func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("tool never entered")
		}
	}
	waitEntered()
	first := <-requests
	if err := r.RequestModel(b); err != nil {
		t.Fatal(err)
	}
	if r.CurrentSelection().Model.ID != a.Model.ID {
		t.Fatal("changed selection inside tool batch")
	}
	if saved, err := r.Store.LastSelection(b.Provider); err != nil || saved == nil || saved.Model.ID != b.Model.ID {
		t.Fatal("explicit choice was not remembered before its boundary", saved, err)
	}
	release <- struct{}{}
	waitEntered()
	second := <-requests
	if first.Selection.Model.ID != a.Model.ID || second.Selection.Model.ID != b.Model.ID {
		t.Fatal(first.Selection, second.Selection)
	}
	if saved, err := r.Store.LastSelection(b.Provider); err != nil || saved == nil || saved.Model.ID != b.Model.ID {
		t.Fatal("applied selection was not remembered", saved, err)
	}
	for _, m := range second.Messages {
		if m.State != nil {
			t.Fatal("A reasoning leaked to B")
		}
	}
	if err := r.RequestModel(fast); err != nil {
		t.Fatal(err)
	}
	// Replaced pending choices must not leave switch records.
	if err := r.RequestModel(a); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn stuck")
	}
	third := <-requests
	if third.Selection.Model.ID != a.Model.ID {
		t.Fatal(third.Selection)
	}
	kept := 0
	for _, m := range third.Messages {
		if m.State == nil {
			continue
		}
		for _, item := range m.State.Items {
			kept++
			if !strings.Contains(string(item), a.Model.ID) {
				t.Fatal("wrong native state retained", string(item))
			}
		}
	}
	if kept != 1 {
		t.Fatal("lost original A reasoning", kept)
	}
	var count int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='model_switch'").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	stored, err := r.Store.Session(r.Current())
	if err != nil || stored.Model.Model.ID != a.Model.ID {
		t.Fatal(stored, err)
	}
	entries, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.Content), `"type":"model_switch"`) && !strings.Contains(r.Store.Label(entry), "Model switched") {
			t.Fatal("switch not replayable", entry)
		}
	}
}

func TestSwitchStorageFailureAndNoOp(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	a := r.CurrentSelection()
	b := a
	b.Model.ID = "new-model"
	if err := r.RequestModel(a); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyModel(""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	seedRuntime(t, r, "Existing conversation for model switch")
	if _, err := r.Store.DB.Exec("CREATE TRIGGER reject_model BEFORE UPDATE OF model_json ON sessions BEGIN SELECT RAISE(ABORT,'model blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestModel(b); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyModel(""); err == nil {
		t.Fatal("ignored switch persistence failure")
	}
	stored, err := r.Store.Session(r.Current())
	if err != nil || stored.Model.Model.ID != a.Model.ID || r.CurrentSelection().Model.ID != a.Model.ID {
		t.Fatal(stored, r.CurrentSelection(), err)
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER reject_model"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ApplyModel(""); err != nil {
		t.Fatal(err)
	}
	if r.CurrentSelection().Model.ID != b.Model.ID {
		t.Fatal("pending selection lost")
	}
}

func TestIdleSwitchReturnsEventWithoutBlockingFullUIQueue(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := make(chan Event, 1)
	queue <- Event{Kind: "job", Text: "full"}
	r.Emit = func(event Event) {
		select {
		case queue <- event:
		case <-ctx.Done():
		}
	}
	next := r.CurrentSelection()
	next.Model.ID = "idle-switch"
	if err := r.RequestModel(next); err != nil {
		t.Fatal(err)
	}
	result := make(chan Event, 1)
	go func() {
		event, err := r.ApplyModel("")
		if err != nil {
			t.Error(err)
		}
		result <- event
	}()
	select {
	case event := <-result:
		if event.Kind != "status" || event.EntryID != 0 || !strings.Contains(event.Text, "Model switched") {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		cancel()
		<-result
		t.Fatal("idle switch tried to emit into its own full UI queue")
	}
}

func TestStandardFastOpaqueCompatibilityAndLoadSelection(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	normal := r.CurrentSelection()
	fast := normal
	fast.Model.BaseID = normal.Model.ID
	fast.Model.ID += "/fast"
	fast.Model.ServiceTier = "priority"
	requests := make(chan provider.Request, 3)
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		requests <- req
		if err := emit(provider.StreamEvent{Kind: "state", StateVersion: 1, StateItem: json.RawMessage(`{"type":"reasoning","encrypted_content":"same-base"}`)}); err != nil {
			return err
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "done"})
	}}
	for i, choice := range []provider.Selection{normal, fast, normal} {
		if err := r.RequestModel(choice); err != nil {
			t.Fatal(err)
		}
		if err := r.Run(&provider.Message{Role: "user", Content: "same base"}); err != nil {
			t.Fatal(err)
		}
		req := <-requests
		kept := 0
		for _, m := range req.Messages {
			if m.State != nil {
				kept += len(m.State.Items)
			}
		}
		if kept != i || req.Selection.Model.ID != choice.Model.ID {
			t.Fatal("tier switch lost compatible reasoning", i, kept, req.Selection)
		}
	}
	other := normal
	other.Model.ID = "saved-other"
	savedID := history.NewID("session")
	turn, _, err := r.Store.StartSession(savedID, r.Workspace.Root, other, provider.Message{Role: "user", Content: "Saved earlier work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + savedID); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Store.Session(savedID)
	if err != nil || loaded.Model.Model.ID != normal.Model.ID || r.CurrentSelection().Model.ID != normal.Model.ID {
		t.Fatal(loaded, r.CurrentSelection(), err)
	}
	entries, err := r.Store.Branch(savedID, 0)
	if err != nil || len(entries) != 2 || !strings.Contains(r.Store.Label(entries[1]), "Model switched") {
		t.Fatal(entries, err)
	}
}

func TestLoadModelFailurePreservesRuntimeTools(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "after-failed-load", Name: "shell", Arguments: []byte(`{"command":"printf still-working"}`)}}}, {Text: "done"}})
	old := r.Current()
	other := r.CurrentSelection()
	other.Model.ID = "different"
	target := history.NewID("session")
	turn, _, err := r.Store.StartSession(target, r.Workspace.Root, other, provider.Message{Role: "user", Content: "Saved target work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec("CREATE TRIGGER reject_load_model BEFORE UPDATE OF model_json ON sessions BEGIN SELECT RAISE(ABORT,'load model blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/load " + target); err == nil {
		t.Fatal("load ignored persistence failure")
	}
	if r.Current() != old {
		t.Fatal("failed load changed session")
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "continue after failed load"}); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE provider_call_id='after-failed-load'").Scan(&result); err != nil || !strings.Contains(result, "still-working") || !strings.Contains(result, `"ok":true`) {
		t.Fatal(result, err)
	}
}
