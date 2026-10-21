package history

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestStorageIntentsWorkspaceAndTip(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	ctx := context.Background()
	path, err := s.WorkspacePath(ctx, v.WorkspaceID)
	if err != nil || path == "" {
		t.Fatal(path, err)
	}
	listed, err := s.Sessions(path)
	if err != nil || len(listed) != 1 || listed[0].ID != v.ID {
		t.Fatal(listed, err)
	}
	saved, err := s.Session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tip, err := s.SessionTip(ctx, v.ID); err != nil || tip != saved.EntryTip {
		t.Fatal(tip, err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=NULL WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	if tip, err := s.SessionTip(ctx, v.ID); err != nil || tip != 0 {
		t.Fatal("empty tip", tip, err)
	}
	for name, read := range map[string]func() error{
		"workspace": func() error { _, e := s.WorkspacePath(ctx, "missing"); return e },
		"session":   func() error { _, e := s.SessionTip(ctx, "missing"); return e },
	} {
		t.Run(name, func(t *testing.T) {
			if err := read(); !errors.Is(err, ErrNotFound) {
				t.Fatal("missing record", err)
			}
		})
	}
}

func TestStorageIntentsNamingClaimsAndConditionalUpdates(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	ctx := context.Background()
	var claimed int
	var mu sync.Mutex
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			ok, err := s.ClaimSessionNaming(ctx, v.ID, turn)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		})
	}
	workers.Wait()
	if claimed != 1 {
		t.Fatalf("granted %d claims, want one", claimed)
	}
	var recorded string
	if err := s.db.QueryRow("SELECT json_extract(metadata_json,'$.naming_turn_id') FROM sessions WHERE id=?", v.ID).Scan(&recorded); err != nil || recorded != turn {
		t.Fatal(recorded, err)
	}
	if ok, err := s.SetAutomaticSessionName(ctx, v.ID, "Generated session title"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.SetAutomaticSessionName(ctx, v.ID, "Another generated title"); err != nil || ok {
		t.Fatal("overwrote automatic name", ok, err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET name='Human title',name_source='manual',naming_claimed=0 WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimSessionNaming(ctx, v.ID, turn); err != nil || ok {
		t.Fatal("claimed manual name", ok, err)
	}
	if ok, err := s.SetAutomaticSessionName(ctx, v.ID, "Late generated title"); err != nil || ok {
		t.Fatal("overwrote manual name", ok, err)
	}
	if saved, err := s.Session(v.ID); err != nil || saved.Name != "Human title" {
		t.Fatal(saved, err)
	}
	if ok, err := s.ClaimSessionNaming(ctx, "missing", turn); err != nil || ok {
		t.Fatal("absent claim", ok, err)
	}
	if ok, err := s.SetAutomaticSessionName(ctx, "missing", "Title"); err != nil || ok {
		t.Fatal("absent update", ok, err)
	}
}

func TestStorageIntentsToolMetadataAndOutcomes(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	ctx := context.Background()
	call, err := s.CallIntent(v.ID, turn, "main", request, llm.ToolCall{ID: "image", Name: "image_show", Arguments: json.RawMessage(`{"path":"fixture.png"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if selection, err := s.ToolSelection(ctx, call); err != nil || !reflect.DeepEqual(selection, v.Model) {
		t.Fatal(selection, err)
	}
	intent, err := s.ToolIntentEntry(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := s.ToolExecution(ctx, call)
	if err != nil || metadata.EntryID != intent || metadata.RequestID != request || !reflect.DeepEqual(metadata.Selection, v.Model) {
		t.Fatal(metadata, err)
	}
	if entry, found, err := s.ToolInspectionEntry(ctx, call); err != nil || !found || entry != intent {
		t.Fatal(entry, found, err)
	}
	if outcome, err := s.ToolOutcomeForEntry(ctx, intent); err != nil || outcome.CallID != call || outcome.Name != "image_show" || outcome.Result != nil {
		t.Fatal("pending outcome", outcome, err)
	}
	// Load copies physical entries but not call identity or its request selection.
	loaded, err := s.Load(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := s.ToolIntentEntry(ctx, call)
	if err != nil || copy <= intent {
		t.Fatal("did not choose latest copy", copy, intent, err)
	}
	if metadata, err := s.ToolExecution(ctx, call); err != nil || metadata.EntryID != copy || metadata.RequestID != request {
		t.Fatal(metadata, err)
	}
	result, err := s.CallResult(loaded.ID, turn, "main", call, json.RawMessage(`null`), nil, map[string]any{"result": json.RawMessage(`null`)}, render.Markdown{Revision: 1, Summary: "image", Detail: "details"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if entry, found, err := s.ToolInspectionEntry(ctx, call); err != nil || !found || entry != result {
		t.Fatal("result did not supersede intent", entry, found, err)
	}
	for _, entry := range []int64{intent, copy, result} {
		outcome, err := s.ToolOutcomeForEntry(ctx, entry)
		if err != nil || outcome.Result == nil || string(outcome.Result) != "null" {
			t.Fatal("completed null result", outcome, err)
		}
		outcome.Result[0] = 'X'
	}
	if entry, found, err := s.ToolInspectionEntry(ctx, "missing"); err != nil || found || entry != 0 {
		t.Fatal("absent optional entry", entry, found, err)
	}
	if _, err := s.ToolIntentEntry(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.ToolSelection(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.ToolExecution(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, entry := range []int64{-1, v.EntryTip} {
		if _, err := s.ToolOutcomeForEntry(ctx, entry); !errors.Is(err, ErrNotFound) {
			t.Fatal("missing or unrelated entry", err)
		}
	}
	if _, err := s.db.Exec("UPDATE model_requests SET model_json='42' WHERE id=?", request); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ToolSelection(ctx, call); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("malformed model accepted", err)
	}
	if metadata, err := s.ToolExecution(ctx, call); err == nil || errors.Is(err, ErrNotFound) || metadata.EntryID != 0 {
		t.Fatal("malformed model accepted", metadata, err)
	}
}

func TestToolOutcomeUsesOriginalExecutionWhileConversationUsesDelivery(t *testing.T) {
	s, v, turn, request := historyFixture(t)
	_, calls, err := s.Assistant(v.ID, turn, "main", request, llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "snapshot", Name: "image_show", Arguments: json.RawMessage(`{"path":"fixture.png"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.ToolIntentEntry(context.Background(), calls[0])
	if err != nil {
		t.Fatal(err)
	}
	original := json.RawMessage(`{"ok":true,"snapshot":"immutable-fixture.png","width":12,"height":8}`)
	delivered := json.RawMessage(`{"ok":false,"reason":"batch_capacity","message":"original retained for inspection"}`)
	result, err := s.CallResult(v.ID, turn, "main", calls[0], delivered, nil, map[string]any{"name": "image_show", "result": original}, render.Markdown{Revision: 1, Summary: "image", Detail: "snapshot fixture"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []int64{intent, result} {
		outcome, err := s.ToolOutcomeForEntry(context.Background(), entry)
		if err != nil || outcome.CallID != calls[0] || outcome.Name != "image_show" || string(outcome.Result) != string(original) {
			t.Fatal("inspection did not recover original snapshot result", outcome, err)
		}
		outcome.Result[0] = 'X'
	}
	messages, err := s.Messages(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if message.Type == llm.TypeToolResult {
			found = true
			if message.Content != string(delivered) {
				t.Fatal("conversation substituted original execution for delivery", message.Content)
			}
		}
	}
	if !found {
		t.Fatal("conversation omitted completed tool result")
	}
}

func TestToolOutcomeRejectsMalformedExecutionRecord(t *testing.T) {
	for _, record := range []any{nil, map[string]any{"other": true}, []string{"not an object"}} {
		s, v, turn, request := historyFixture(t)
		call, err := s.CallIntent(v.ID, turn, "main", request, llm.ToolCall{ID: "snapshot", Name: "image_show", Arguments: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := s.CallResult(v.ID, turn, "main", call, json.RawMessage(`{"notice":true}`), nil, record, render.Markdown{Revision: 1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ToolOutcomeForEntry(context.Background(), entry); err == nil {
			t.Fatal("malformed execution record fell back to model delivery", record)
		}
	}
}

func TestStorageIntentsCancellationAndStorageErrors(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	operations := map[string]func(context.Context) error{
		"workspace": func(ctx context.Context) error { _, e := s.WorkspacePath(ctx, v.WorkspaceID); return e },
		"tip":       func(ctx context.Context) error { _, e := s.SessionTip(ctx, v.ID); return e },
		"claim":     func(ctx context.Context) error { _, e := s.ClaimSessionNaming(ctx, v.ID, turn); return e },
		"name": func(ctx context.Context) error {
			_, e := s.SetAutomaticSessionName(ctx, v.ID, "Generated title")
			return e
		},
		"selection":  func(ctx context.Context) error { _, e := s.ToolSelection(ctx, "missing"); return e },
		"execution":  func(ctx context.Context) error { _, e := s.ToolExecution(ctx, "missing"); return e },
		"intent":     func(ctx context.Context) error { _, e := s.ToolIntentEntry(ctx, "missing"); return e },
		"inspection": func(ctx context.Context) error { _, _, e := s.ToolInspectionEntry(ctx, "missing"); return e },
		"outcome":    func(ctx context.Context) error { _, e := s.ToolOutcomeForEntry(ctx, -1); return e },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, operation := range operations {
		t.Run(name+" canceled", func(t *testing.T) {
			if err := operation(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	if ok, err := s.ClaimSessionNaming(context.Background(), v.ID, turn); err != nil || !ok {
		t.Fatal("canceled claim mutated storage", ok, err)
	}
	if saved, err := s.Session(v.ID); err != nil || saved.Name != "New session" {
		t.Fatal("canceled naming mutated storage", saved, err)
	}
	// Operations waiting for the Store pool honor deadlines, not only contexts
	// canceled before entry.
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := s.SessionTip(deadline, v.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("blocked read ignored deadline", err)
	}
	connection.Close()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_naming BEFORE UPDATE OF name ON sessions BEGIN SELECT RAISE(ABORT,'injected naming failure'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetAutomaticSessionName(context.Background(), v.ID, "Generated title"); ok || err == nil || !strings.Contains(err.Error(), "injected naming failure") {
		t.Fatal("write failure hidden", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, operation := range operations {
		t.Run(name+" closed", func(t *testing.T) {
			if err := operation(context.Background()); err == nil || errors.Is(err, ErrNotFound) {
				t.Fatal("storage error hidden as absence", err)
			}
		})
	}
}
