package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
	"scicode/internal/tool"
	"scicode/internal/workspace"
)

// childProvider retains the fixture's catalog/login behavior and controls streams.
type childProvider struct {
	provider.Script
	stream func(context.Context, provider.Request, func(provider.StreamEvent) error) error
}

func TestSubagentRequiresConciseTitle(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, label := range []string{"", "   ", strings.Repeat("界", 65), "two\nlines", "two\u2028lines", "tab\ttitle", "bad\x1btitle", "one two three four five"} {
		args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": label})
		record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "invalid"}, "subagent", args)
		if !strings.Contains(string(record.Result), "invalid_arguments") {
			t.Fatal(label, string(record.Result))
		}
	}
}

func (p *childProvider) Stream(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
	return p.stream(ctx, request, emit)
}

func TestForegroundChildSharedUndoAndInspectableTools(t *testing.T) {
	r, events := runtimeFixture(t, []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"write the result","label":"writer"}`)}}},
		{Calls: []provider.ToolCall{{ID: "write", Name: "write", Arguments: []byte(`{"path":"child.txt","content":"result\n"}`)}}},
		{Text: "Child finished."}, {Text: "Parent finished."},
	})
	m := provider.Message{Role: "user", Content: "Private parent context"}
	if err := r.Run(&m); err != nil {
		t.Fatal(err)
	}
	if totals := r.UsageSnapshot().Totals; totals.Requests != 4 || totals.ReportedRequests != 4 {
		t.Fatal("child responses missing from parent totals", totals)
	}
	var childTool bool
	for len(events) > 0 {
		e := <-events
		if e.Kind == "tool" && strings.HasPrefix(e.Actor, "main/child") {
			entry, err := r.Store.Entry(e.EntryID)
			if err != nil || entry.Kind != "tool_result" || entry.Visible || entry.Actor == "main" {
				t.Fatal(entry, err)
			}
			text, err := r.Store.Inspect(entry)
			if err != nil || !strings.Contains(text, "child.txt") {
				t.Fatal(text, err)
			}
			childTool = true
		}
	}
	if !childTool {
		t.Fatal("missing live child tool inspector")
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.Contains(message.Content, "Child finished.") && message.Role == "assistant" {
			t.Fatal("child transcript leaked into parent", message)
		}
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Root, "child.txt")); !os.IsNotExist(err) {
		t.Fatal("child write escaped parent undo", err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(r.Workspace.Root, "child.txt")); err != nil || string(data) != "result\n" {
		t.Fatal(string(data), err)
	}
}

func TestBackgroundChildFrozenSelectionAcrossCompaction(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.selection.Model.Budget.RecentTokensTarget = 256
	frozen := r.selection
	started, release := make(chan struct{}), make(chan struct{})
	childCycle := 0
	p := &childProvider{}
	p.stream = func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		if strings.Contains(req.System, "\nYou are an isolated child agent.") {
			var latest runtimeContext
			for _, message := range req.Messages {
				if message.Role == "developer" && message.Runtime {
					if err := json.Unmarshal([]byte(message.Content), &latest); err != nil {
						return err
					}
				}
			}
			if req.Selection.Model.ID != frozen.Model.ID || !strings.Contains(latest.Model, frozen.Model.ID+"/none") {
				t.Error("child selection changed", req.Selection, req.System)
			}
			if strings.Contains(req.Messages[0].Content, "Private parent") {
				t.Error("child inherited parent transcript")
			}
			childCycle++
			if childCycle == 1 {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{Attempt: 2, DelayMilliseconds: 1000, Reason: "HTTP 503"}}); err != nil {
					return err
				}
				call := provider.ToolCall{ID: "write", Name: "write", Arguments: []byte(`{"path":"child-after-compact.txt","content":"safe\n"}`)}
				return emit(provider.StreamEvent{Kind: "call", Call: &call})
			}
			return emit(provider.StreamEvent{Kind: "text", Text: "Done after compaction."})
		}
		if req.NoTools {
			if err := emit(provider.StreamEvent{Kind: "retry", Retry: &provider.Retry{Attempt: 2, Reason: "HTTP 429"}}); err != nil {
				return err
			}
			return emit(provider.StreamEvent{Kind: "text", Text: "Keep the child task and research state."})
		}
		last := req.Messages[len(req.Messages)-2]
		if last.Role == "user" && strings.HasPrefix(last.Content, "Private parent") {
			call := provider.ToolCall{ID: "child", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"write after gate","label":"background writer","background":true}`)}
			return emit(provider.StreamEvent{Kind: "call", Call: &call})
		}
		return emit(provider.StreamEvent{Kind: "text", Text: "Parent done."})
	}
	r.Provider = p
	// Keep compaction pressure independent of generated identifier lengths.
	m := provider.Message{Role: "user", Content: "Private parent starts a child\n" + strings.Repeat("research context ", 128)}
	if err := r.Run(&m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("child not started")
	}
	changed := r.CurrentSelection()
	changed.Model.ID = "changed"
	if err := r.RequestModel(changed); err != nil {
		t.Fatal(err)
	}
	m.Content = "Next parent turn"
	if err := r.Run(&m); err != nil {
		t.Fatal(err)
	}
	before := r.Current()
	// Occupy the shared file queue during continuation. This models a child
	// Transform already in flight: the routing lock must cover its commit.
	gate, finish := make(chan struct{}), make(chan struct{})
	mutation := make(chan error, 1)
	var callID string
	if err := r.Store.DB.QueryRow("SELECT id FROM tool_calls WHERE name='subagent'").Scan(&callID); err != nil {
		t.Fatal(err)
	}
	go func() {
		r.routeMu.RLock()
		defer r.routeMu.RUnlock()
		_, err := r.Workspace.Apply(context.Background(), r.Current(), callID, []workspace.Mutation{{Path: "gate.txt", Transform: func([]byte) ([]byte, error) { close(gate); <-finish; return []byte("gate\n"), nil }}})
		mutation <- err
	}()
	<-gate
	compacted := make(chan error, 1)
	go func() { _, err := r.Command("/compact"); compacted <- err }()
	select {
	case err := <-compacted:
		close(finish)
		t.Fatal("continuation crossed active mutation", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-mutation; err != nil {
		t.Fatal(err)
	}
	if err := <-compacted; err != nil {
		t.Fatal(err)
	}
	if r.Current() == before {
		t.Fatal("compaction did not continue")
	}
	close(release)
	list := r.Jobs.List("main", true)
	if len(list) != 1 {
		t.Fatal(list)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	v, err := r.Jobs.Wait(ctx, "main", list[0].ID, nil)
	if err != nil || v.Status != "completed" {
		t.Fatal(v, err)
	}
	var routed string
	if err := r.Store.DB.QueryRow("SELECT session_id FROM entries WHERE json_extract(content_json,'$.type')='model_retry' AND actor_id != 'main'").Scan(&routed); err != nil || routed != r.Current() {
		t.Fatal("child retry did not follow continuation", routed, err)
	}
	var purpose string
	if err := r.Store.DB.QueryRow("SELECT q.purpose FROM entries e JOIN model_requests q ON q.id=json_extract(e.content_json,'$.request_id') WHERE json_extract(e.content_json,'$.type')='model_retry' AND e.actor_id='main' LIMIT 1").Scan(&purpose); err != nil || purpose != "compaction" {
		t.Fatal("compaction retry missing", purpose, err)
	}
	data, err := os.ReadFile(filepath.Join(r.Workspace.Root, "child-after-compact.txt"))
	if err != nil || string(data) != "safe\n" {
		t.Fatal(string(data), err)
	}
	var raw string
	if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE name='write'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || !result.OK {
		t.Fatal(raw, err)
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Root, "child-after-compact.txt")); !os.IsNotExist(err) {
		t.Fatal("undo after continuation", err)
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Root, "child-after-compact.txt")); err != nil {
		t.Fatal("redo after continuation", err)
	}
}
