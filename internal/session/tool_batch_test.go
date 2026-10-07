package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/llm"
	"ttc/internal/tool"
)

func batchIntents(t *testing.T, r *Runtime, actor string, calls []llm.ToolCall) (string, []string) {
	t.Helper()
	r.mu.Lock()
	persisted := r.persisted
	r.mu.Unlock()
	if !persisted {
		seedRuntime(t, r, "Research request for tool batch")
	}
	turn, err := r.Store.BeginTurn(r.Current(), "user", r.selection)
	if err != nil {
		t.Fatal(err)
	}
	request, err := r.Store.StartRequest(r.Current(), turn, actor, "coding", r.selection)
	if err != nil {
		t.Fatal(err)
	}
	_, ids, err := r.Store.Assistant(r.Current(), turn, actor, request, llm.Message{Role: "assistant", Calls: calls})
	if err != nil {
		t.Fatal(err)
	}
	return turn, ids
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func TestToolBatchOverlapsReadsShellsAndOrderedWrites(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Tools = tool.NewRegistry()
	started := make(chan string, 8)
	completed := make(chan string, 8)
	r.Emit = func(e Event) {
		if e.Kind == "tool" {
			completed <- e.Text
		}
	}
	gates := map[string]chan struct{}{}
	for _, id := range []string{"read1", "read2", "slow", "fast"} {
		gates[id] = make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-gates[id]:
			default:
				close(gates[id])
			}
		})
	}
	type args struct {
		ID string `json:"id"`
	}
	for _, name := range []string{"read", "shell", "write"} {
		tool.Register(r.Tools, name, "gated test tool", map[string]any{"id": tool.Property("string")}, []string{"id"}, func(args) error { return nil }, func(ctx context.Context, x tool.Execution, a args) (any, error) {
			started <- a.ID
			if gate := gates[a.ID]; gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return map[string]any{"value": a.ID}, nil
		})
	}
	calls := []llm.ToolCall{}
	for _, pair := range [][2]string{{"write", "write1"}, {"shell", "slow"}, {"read", "read1"}, {"missing", "unknown"}, {"write", "write2"}, {"shell", "fast"}, {"read", "read2"}} {
		calls = append(calls, llm.ToolCall{ID: pair[1], Name: pair[0], Arguments: []byte(fmt.Sprintf(`{"id":%q}`, pair[1]))})
	}
	turn, ids := batchIntents(t, r, "main", calls)
	type outcome struct {
		records []tool.Record
		err     error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		records, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil)
		done <- outcome{records, err}
	}()
	first := map[string]bool{}
	for range 4 {
		first[receive(t, started)] = true
	}
	if !first["slow"] || !first["fast"] || !first["read1"] || !first["read2"] {
		t.Fatal("read/shell calls did not overlap", first)
	}
	close(gates["fast"])
	receive(t, completed)
	close(gates["read2"])
	receive(t, completed)
	select {
	case v := <-started:
		t.Fatal("write started before all reads finished", v)
	default:
	}
	close(gates["read1"])
	if value := receive(t, started); value != "write1" {
		t.Fatal(value)
	}
	if value := receive(t, started); value != "write2" {
		t.Fatal(value)
	}
	select {
	case <-done:
		t.Fatal("batch returned before slow shell finished")
	default:
	}
	close(gates["slow"])
	result := receive(t, done)
	if result.err != nil || len(result.records) != len(calls) {
		t.Fatal(result)
	}
	for i, record := range result.records {
		if record.Name != calls[i].Name || string(record.Arguments) != string(calls[i].Arguments) {
			t.Fatal("result association lost", i, record)
		}
		var stored string
		if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", ids[i]).Scan(&stored); err != nil || stored != string(record.Result) {
			t.Fatal(stored, record, err)
		}
	}
	if !strings.Contains(string(result.records[3].Result), "unknown_tool") {
		t.Fatal("unknown tool did not fail explicitly")
	}
}

func TestToolBatchCancellationSkipsQueuedWritesAndJoins(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.Tools = tool.NewRegistry()
	started, joined := make(chan struct{}, 1), make(chan struct{}, 1)
	tool.Register(r.Tools, "read", "wait", nil, nil, func(struct{}) error { return nil }, func(ctx context.Context, _ tool.Execution, _ struct{}) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		joined <- struct{}{}
		return nil, ctx.Err()
	})
	var writes atomic.Int32
	tool.Register(r.Tools, "write", "must not execute", nil, nil, func(struct{}) error { return nil }, func(context.Context, tool.Execution, struct{}) (any, error) { writes.Add(1); return nil, nil })
	calls := []llm.ToolCall{{ID: "w", Name: "write", Arguments: []byte(`{}`)}, {ID: "r", Name: "read", Arguments: []byte(`{}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil); done <- err }()
	receive(t, started)
	cancel()
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	receive(t, joined)
	if writes.Load() != 0 {
		t.Fatal("cancelled queued write executed")
	}
	for _, id := range ids {
		var result string
		if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", id).Scan(&result); err != nil || !strings.Contains(result, "cancelled") {
			t.Fatal(result, err)
		}
	}
}

func TestToolBatchInterruptedStreamExecutesNothing(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	calls := []llm.ToolCall{{ID: "w", Name: "write", Arguments: []byte(`{"path":"x","content":"wrong"}`)}, {ID: "s", Name: "shell", Arguments: []byte(`{"command":"touch should-not-exist"}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, errors.New("broken SSE"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if !strings.Contains(string(record.Result), "interrupted") {
			t.Fatal(record)
		}
	}
	for _, path := range []string{"x", "should-not-exist"} {
		if _, err := os.Stat(filepath.Join(r.Workspace.Root, path)); !os.IsNotExist(err) {
			t.Fatal(path, err)
		}
	}
}

func TestToolBatchStorageFailureCancelsSiblingsAndQueuedWrites(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.Tools = tool.NewRegistry()
	var writes atomic.Int32
	started := make(chan struct{}, 1)
	tool.Register(r.Tools, "shell", "wait for failure", nil, nil, func(struct{}) error { return nil }, func(ctx context.Context, _ tool.Execution, _ struct{}) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tool.Register(r.Tools, "write", "count writes", nil, nil, func(struct{}) error { return nil }, func(context.Context, tool.Execution, struct{}) (any, error) {
		receive(t, started)
		writes.Add(1)
		return nil, nil
	})
	calls := []llm.ToolCall{{ID: "first", Name: "write", Arguments: []byte(`{}`)}, {ID: "second", Name: "write", Arguments: []byte(`{}`)}, {ID: "sibling", Name: "shell", Arguments: []byte(`{}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_first_result BEFORE UPDATE OF result_json ON tool_calls WHEN OLD.provider_call_id='first' BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
		done <- err
	}()
	if err := receive(t, done); err == nil || !strings.Contains(err.Error(), "injected storage failure") {
		t.Fatal(err)
	}
	if writes.Load() != 1 {
		t.Fatal("queued write escaped fatal persistence failure", writes.Load())
	}
	for _, id := range ids[1:] {
		var result string
		if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE id=?", id).Scan(&result); err != nil || !strings.Contains(result, "cancelled") {
			t.Fatal(result, err)
		}
	}
}

func TestTurnsContinueBeyond64Cycles(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%t", child), func(t *testing.T) {
			responses := []llm.ScriptResponse{}
			if child {
				responses = append(responses, llm.ScriptResponse{Calls: []llm.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"continue","label":"long child"}`)}}})
			}
			for i := range 70 {
				responses = append(responses, llm.ScriptResponse{Calls: []llm.ToolCall{{ID: fmt.Sprintf("cycle%d", i), Name: "wakeup_list", Arguments: []byte(`{}`)}}})
			}
			responses = append(responses, llm.ScriptResponse{Text: "Finished after 70 cycles."})
			if child {
				responses = append(responses, llm.ScriptResponse{Text: "Parent finished."})
			}
			r, _ := runtimeFixture(t, responses)
			r.Emit = nil
			m := llm.Message{Role: "user", Content: "continue"}
			if err := r.Run(&m); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM tool_calls WHERE name='wakeup_list' AND result_json IS NOT NULL").Scan(&count); err != nil || count != 70 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestChildParallelAdmissionAndSlotRelease(t *testing.T) {
	for _, asides := range []int{0, 1} {
		t.Run(fmt.Sprintf("asides=%d", asides), func(t *testing.T) {
			testChildParallelAdmissionAndSlotRelease(t, asides)
		})
	}
}

func testChildParallelAdmissionAndSlotRelease(t *testing.T, asides int) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started := make(chan struct{}, maxChildSlots+2)
	release := make(chan struct{})
	defer close(release)
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		started <- struct{}{}
		select {
		case <-release:
			return emit(llm.StreamEvent{Kind: "text", Text: "done"})
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	seedRuntime(t, r, "shared child capacity")
	for range asides {
		if _, err := r.StartBTW("hold"); err != nil {
			t.Fatal(err)
		}
		receive(t, started)
	}
	calls := []llm.ToolCall{}
	for i := range maxChildSlots + 2 {
		calls = append(calls, llm.ToolCall{ID: fmt.Sprint(i), Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"hold","label":"held child","background":true}`)})
	}
	turn, ids := batchIntents(t, r, "main", calls)
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted, denied := 0, 0
	var jobID string
	for _, record := range records {
		var result struct {
			OK    bool        `json:"ok"`
			ID    string      `json:"job_id"`
			Error *tool.Error `json:"error"`
		}
		if err := json.Unmarshal(record.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.OK {
			accepted++
			jobID = result.ID
		} else if result.Error != nil && result.Error.Code == "capacity" {
			denied++
		} else {
			t.Fatal(string(record.Result))
		}
	}
	if accepted != maxChildSlots-asides || denied != 2+asides {
		t.Fatal(accepted, denied)
	}
	if _, err := r.StartBTW("overflow"); err == nil {
		t.Fatal("aside exceeded shared capacity")
	}
	for range maxChildSlots - asides {
		receive(t, started)
	}
	if _, err := r.Jobs.Stop("main", jobID); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	calls = calls[:1]
	calls[0].ID = "replacement"
	turn, ids = batchIntents(t, r, "main", calls)
	records, err = r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil || !strings.Contains(string(records[0].Result), `"ok":true`) {
		t.Fatal(records, err)
	}
	receive(t, started)
	if len(r.Jobs.List("main", false)) != maxChildSlots {
		t.Fatal("released slot did not admit exactly one child")
	}
}

func TestStaticSystemPromptLinesAreAtMost80Columns(t *testing.T) {
	for _, line := range strings.Split(systemTemplate, "\n") {
		if len(line) > 80 {
			t.Fatal("overlong static prompt line", len(line), line)
		}
	}
	r, _ := runtimeFixture(t, nil)
	long := strings.Repeat("instruction ", 30)
	if err := os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte(long), 0600); err != nil {
		t.Fatal(err)
	}
	message, _, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil || !strings.Contains(message.Content, long) {
		t.Fatal("injected instructions were reformatted", err)
	}
}

func TestChildContextRetainsCallOrderAfterOutOfOrderCompletion(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Research request for parallel child")
	r.Tools = tool.NewRegistry()
	started, slowRelease := make(chan struct{}), make(chan struct{})
	tool.Register(r.Tools, "shell", "controlled child tool", map[string]any{"slow": tool.Property("boolean")}, nil,
		func(a struct {
			Slow bool `json:"slow"`
		}) error {
			return nil
		},
		func(ctx context.Context, _ tool.Execution, a struct {
			Slow bool `json:"slow"`
		}) (any, error) {
			gate := started
			if a.Slow {
				close(started)
				gate = slowRelease
			}
			select {
			case <-gate:
				return map[string]any{"slow": a.Slow}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
	contextUpdates := 0
	r.Emit = func(e Event) {
		if e.Kind == "runtime_context" {
			contextUpdates++
			if e.EntryID == 0 {
				t.Error("empty child context UI row")
			}
		}
		if e.Kind != "tool" {
			return
		}
		var result string
		if err := r.Store.DB.QueryRow("SELECT c.result_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE r.entry_id=?", e.EntryID).Scan(&result); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(result, `"slow":false`) {
			close(slowRelease)
		}
	}
	cycle := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		cycle++
		if cycle == 1 {
			for _, call := range []llm.ToolCall{{ID: "slow", Name: "shell", Arguments: []byte(`{"slow":true}`)}, {ID: "fast", Name: "shell", Arguments: []byte(`{"slow":false}`)}} {
				if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
			}
			return nil
		}
		if len(req.Messages) != 5 || req.Messages[3].CallID != "slow" || req.Messages[4].CallID != "fast" {
			return fmt.Errorf("child context lost call order: %+v", req.Messages)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "done"})
	}}
	turn, err := r.Store.BeginTurn(r.Current(), "user", r.selection)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.runChild(ctx, childTask{actor: "main/child-test", turn: turn, prompt: "parallel child", selection: r.selection, tools: r.Tools}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if contextUpdates != 1 {
		t.Fatal("unchanged child snapshot generated UI updates", contextUpdates)
	}
	rows, err := r.Store.DB.Query("SELECT c.provider_call_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id ORDER BY r.entry_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "fast,slow" {
		t.Fatal("completion order not persisted", order)
	}
}
