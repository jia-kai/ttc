package history

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/render"
)

func TestReadReplayDoesNotDuplicateMessageBodiesInLabels(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	body := strings.Repeat("Large canonical message \\\n", 8192)
	want := make(map[int64]llm.Message)
	for _, fixture := range []struct {
		kind string
		typ  llm.MessageType
	}{
		{"message", llm.TypeUserInput},
		{"message", llm.TypeAssistant},
		{"summary", llm.TypeAssistant},
	} {
		message := llm.Message{Type: fixture.typ, Content: body}
		id, err := s.Append(session.ID, turn, "main", fixture.kind, message.Role(), true, message)
		if err != nil {
			t.Fatal(err)
		}
		want[id] = message
	}
	replay, err := s.ReadReplay(context.Background(), session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range replay.Rows {
		if row.Message != nil && row.Label != "" {
			t.Fatalf("message row #%d retained an unused %d-byte label", row.ID, len(row.Label))
		}
		if message, found := want[row.ID]; found {
			if row.Message == nil || row.Message.Content != message.Content || row.Message.Type != message.Type {
				t.Fatalf("canonical message #%d changed", row.ID)
			}
			delete(want, row.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("replay lost %d canonical messages", len(want))
	}
}

func TestReadReplayToolSupersessionStaysWithinCut(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	call, err := s.CallIntent(session.ID, turn, "main", request, llm.ToolCall{ID: "provider", Name: "read", Arguments: json.RawMessage(`{"path":"fixture"}`)})
	if err != nil {
		t.Fatal(err)
	}
	cut := streamTip(t, s, session.ID)
	before, err := s.ReadReplay(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	md := render.Markdown{Revision: 1, Summary: "Saved result", Detail: strings.Repeat("large details", 10000)}
	result, err := s.CallResult(session.ID, turn, "main", call, json.RawMessage(`{"ok":true}`), nil, nil, md, true)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := s.ReadReplay(ctx, session.ID, cut)
	if err != nil || !reflect.DeepEqual(frozen, before) {
		t.Fatalf("later stored result changed frozen pending presentation: before=%+v frozen=%+v err=%v", before, frozen, err)
	}
	findTools := func(replay Replay) []ReplayRow {
		var out []ReplayRow
		for _, row := range replay.Rows {
			if row.ToolCallID != "" {
				out = append(out, row)
			}
		}
		return out
	}
	pending := findTools(frozen)
	if len(pending) != 1 || pending[0].ID != cut || pending[0].ToolCallID != call || pending[0].ToolComplete || !pending[0].Execution || !strings.Contains(pending[0].Label, "read") || strings.Contains(pending[0].Label, "tool_call") {
		t.Fatalf("pending tool lacks typed presentation: %+v", pending)
	}
	current, err := s.ReadReplay(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	completed := findTools(current)
	if len(completed) != 1 || completed[0].ID != result || completed[0].ToolCallID != call || !completed[0].ToolComplete || !completed[0].Execution || completed[0].Label != md.Summary {
		t.Fatalf("selected result did not supersede intent: %+v", completed)
	}
}

func TestReadReplayOrdersStreamsAndFreezesGroupedRevisions(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	save := func(direction string) messagestream.Item {
		t.Helper()
		items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{
			Attempt: 1, Direction: direction, Payload: json.RawMessage(`{}`), Inspection: testInspection(direction),
		})
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		return items[0]
	}
	header := save("request")
	cut, err := s.Append(session.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Before response"})
	if err != nil {
		t.Fatal(err)
	}
	response := save("response")
	recordStream(t, s, session, turn, "main", "later", `{}`)
	frozen, err := s.ReadReplay(ctx, session.ID, cut)
	if err != nil {
		t.Fatal(err)
	}
	var streams []messagestream.Item
	for i, row := range frozen.Rows {
		if i > 0 && frozen.Rows[i-1].ID >= row.ID {
			t.Fatalf("rows not in display order: %+v", frozen.Rows)
		}
		if row.Stream != nil {
			streams = append(streams, *row.Stream)
		}
	}
	if !reflect.DeepEqual(streams, []messagestream.Item{header}) {
		t.Fatalf("replay admitted a later header or revision: %+v", streams)
	}
	current, err := s.ReadReplay(ctx, session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	streams = nil
	for _, row := range current.Rows {
		if row.Stream != nil {
			streams = append(streams, *row.Stream)
		}
	}
	if len(streams) != 2 || !reflect.DeepEqual(streams[0], response) {
		t.Fatalf("current replay lost grouped revision or newer header: %+v", streams)
	}
}

func TestReadReplayFiltersStorageRecordsAndNormalizesInputTime(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	if err := s.RenameSession(session.ID, "Replay fixture"); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []llm.MessageType{llm.TypeRuntimeContext, llm.TypeCompactionContext, llm.TypeSystemInstruction, llm.TypeDeveloperInstruction} {
		message := llm.Message{Type: typ, Content: "not a transcript row"}
		if _, err := s.Append(session.ID, turn, "main", "message", message.Role(), true, message); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(session.ID, turn, "main", "status", "", false, map[string]string{"type": "request_finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestMessages(session.ID, turn, "coding", request, []llm.Message{{Type: llm.TypeAssistant, Content: "not a transcript row"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordSystemPrompt(session.ID, turn, "main", request, "not a transcript row"); err != nil {
		t.Fatal(err)
	}
	job, err := s.Append(session.ID, turn, "main", "status", "", false, map[string]any{"type": "job_completion", "markdown": render.Markdown{Revision: 1, Summary: "Job done", Detail: "not loaded"}})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ReadReplay(ctx, session.ID, 0)
	if err != nil || replay.Name != "Replay fixture" || len(replay.Rows) != 2 {
		t.Fatalf("unexpected replay rows: %+v err=%v", replay, err)
	}
	input, completion := replay.Rows[0], replay.Rows[1]
	entry, err := s.Entry(input.ID)
	if err != nil || input.Message == nil || input.Message.Type != llm.TypeUserInput || input.Message.InputTimeMS != entry.CreatedMS {
		t.Fatalf("input admission timestamp lost: %+v err=%v", input, err)
	}
	if completion.ID != job || completion.Label != "Job done" || !completion.Execution || completion.ToolCallID != "" {
		t.Fatalf("job completion metadata lost: %+v", completion)
	}
}

func TestReadReplayRejectsInvalidCutsAndHandlesEmptySession(t *testing.T) {
	s, session, _, _ := historyFixture(t)
	ctx := context.Background()
	other, _ := startHistorySession(t, s, t.TempDir(), session.Model, "other")
	for _, tc := range []struct {
		session string
		tip     int64
	}{{session.ID, -1}, {session.ID, other.EntryTip}, {"missing", 0}, {session.ID, 1 << 50}} {
		if _, err := s.ReadReplay(ctx, tc.session, tc.tip); err == nil {
			t.Fatalf("invalid replay cut accepted: %+v", tc)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ReadReplay(canceled, session.ID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("replay ignored cancellation: %v", err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=NULL WHERE id=?", session.ID); err != nil {
		t.Fatal(err)
	}
	replay, err := s.ReadReplay(ctx, session.ID, 0)
	if err != nil || len(replay.Rows) != 0 {
		t.Fatalf("empty session replayed old entries: %+v err=%v", replay, err)
	}
}

func TestReplayToolResultsSelectsOnlyLatestSelectedResult(t *testing.T) {
	entries := []Entry{
		{ID: 1, Kind: "tool_call", Content: []byte(`{"call_id":"call"}`)},
		{ID: 2, Kind: "tool_result", Content: []byte(`{"call_id":"call"}`)},
		{ID: 3, Kind: "tool_result", Content: []byte(`{"call_id":"call"}`)},
	}
	ids, results, err := replayToolResults(entries[:1])
	if err != nil || ids[1] != "call" || results["call"] != 0 {
		t.Fatalf("result beyond selected cut superseded intent: %v %v %v", ids, results, err)
	}
	_, results, err = replayToolResults(entries)
	if err != nil || results["call"] != 3 {
		t.Fatalf("latest selected result not chosen: %v %v", results, err)
	}
	for _, content := range []string{`{`, `{}`, `{"call_id":""}`} {
		if _, _, err := replayToolResults([]Entry{{ID: 4, Kind: "tool_call", Content: []byte(content)}}); err == nil {
			t.Fatalf("invalid tool reference accepted: %s", content)
		}
	}
}
