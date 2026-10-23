package session

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestLoadJoinsLateChildMutation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Current task")
	target := history.Session{ID: history.NewID("session")}
	turn, _, err := r.Store.StartSession(target.ID, r.Workspace.Root, r.selection, llm.Message{Type: llm.TypeUserInput, Content: "Target history"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	mainAdmitted := make(chan struct{})
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseMutation := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseMutation) // Unblock the worker before runtime cleanup on failure.
	type empty struct{}
	tool.Register(r.Tools, "late_change", "Controlled in-flight mutation", nil, nil, func(empty) error { return nil }, func(ctx context.Context, x tool.Execution, _ empty) (any, error) {
		// The main actor's next request must pass workspace admission before
		// this worker holds the gate until after Run returns.
		select {
		case <-mainAdmitted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		var change int64
		err := r.Workspace.Admit(ctx, func() error {
			close(started)
			select {
			case <-ctx.Done():
				close(canceled)
				<-release // A filesystem operation already beyond cancellation.
			case <-release:
			}
			var err error
			change, err = r.Store.CommitChange(x.SessionID, x.CallID, []string{}, false)
			return err
		})
		return map[string]any{"change_id": change}, err
	})
	mainStep := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if req.ConversationID != r.Current() {
			for _, m := range req.Messages {
				if m.Type == llm.TypeToolResult {
					return emit(llm.StreamEvent{Kind: "text", Text: "Finished"})
				}
			}
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "late", Name: "late_change", Arguments: json.RawMessage(`{}`)}})
		}
		mainStep++
		if mainStep == 1 {
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "child", Name: "subagent", Arguments: json.RawMessage(`{"persistent":true,"prompt":"Write later","label":"Late mutation","background":true}`)}})
		}
		if mainStep == 2 {
			close(mainAdmitted)
		}
		select {
		case <-started:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Child remains running"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start child"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Command("/load " + target.ID); done <- err }()
	// Session activation must cancel and join the in-flight worker before returning.
	select {
	case <-canceled:
	case err := <-done:
		t.Fatal("load crossed live mutation", err)
	case <-time.After(3 * time.Second):
		t.Fatal("load failed to cancel child")
	}
	select {
	case err := <-done:
		t.Fatal("load crossed live mutation", err)
	default:
	}
	releaseMutation()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("load failed to join child")
	}
	loaded, err := r.Store.Session(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID == target.ID || loaded.UndoFloor != loaded.EntryTip {
		t.Fatal("load did not create an independent undo boundary", loaded)
	}
}
