package session

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/tool"
)

func TestLoadFloorReflectsJoinedLateChildMutation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Current task")
	target := history.Session{ID: history.NewID("session")}
	turn, _, err := r.Store.StartSession(target.ID, r.Workspace.Root, r.selection, provider.Message{Role: "user", Content: "Target history"})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	type empty struct{}
	tool.Register(r.Tools, "late_change", "Controlled in-flight mutation", nil, nil, func(empty) error { return nil }, func(ctx context.Context, x tool.Execution, _ empty) (any, error) {
		var change int64
		err := r.Workspace.Admit(ctx, func() error {
			close(started)
			<-release // Represents a filesystem operation already beyond cancellation.
			var err error
			change, err = r.Store.CommitChange(x.SessionID, x.CallID, []string{}, false)
			return err
		})
		return map[string]any{"change_id": change}, err
	})
	mainStep := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if req.ConversationID != r.Current() {
			for _, m := range req.Messages {
				if m.Role == "tool" {
					return emit(provider.StreamEvent{Kind: "text", Text: "Finished"})
				}
			}
			return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "late", Name: "late_change", Arguments: json.RawMessage(`{}`)}})
		}
		mainStep++
		if mainStep == 1 {
			return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "child", Name: "subagent", Arguments: json.RawMessage(`{"prompt":"Write later","label":"Late mutation","background":true}`)}})
		}
		select {
		case <-started:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Child remains running"})
	}}
	if err := r.Run(&provider.Message{Role: "user", Content: "Start child"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Command("/load " + target.ID); done <- err }()
	// Session activation must join the in-flight worker before applying the floor.
	select {
	case err := <-done:
		t.Fatal("load crossed live mutation", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("load failed to join child")
	}
	loaded, err := r.Store.Session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var generation int64
	if err := r.Store.DB.QueryRow("SELECT generation FROM workspaces WHERE id=?", loaded.WorkspaceID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || loaded.Generation != generation || loaded.UndoFloor != loaded.EntryTip {
		t.Fatal("load floor predates joined mutation", loaded, generation)
	}
}
