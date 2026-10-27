package session

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func waitForAwait(t *testing.T, events <-chan Event, actor string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind == "awaiting" && event.Actor == actor {
				return
			}
		case <-timeout:
			t.Fatal("actor never entered awaiting state")
		}
	}
}

func TestAwaitDefersResultUntilUserInputResumes(t *testing.T) {
	r, events := runtimeFixture(t, []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}},
		{Text: "Resumed answer"},
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "wait for input"}) }()
	waitForAwait(t, events, "main")
	var pending int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM tool_calls WHERE name='await' AND result_json IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatal("await result saved before waking", pending)
	}
	select {
	case err := <-done:
		t.Fatal("await completed assignment", err)
	default:
	}
	if err := r.Steer(contextbuild.Input{Text: "continue now"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("input did not resume inference")
	}
	if r.Awaiting("main") {
		t.Fatal("awaiting state survived completion")
	}
	var result string
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT result_json FROM tool_calls WHERE name='await'`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	assertAwaitResult(t, result, true)
}

func assertAwaitResult(t *testing.T, result string, ok bool) float64 {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(result), &value); err != nil {
		t.Fatal(err)
	}
	seconds, hasTime := value["wait_time_seconds"].(float64)
	if value["ok"] != ok || !hasTime || seconds < 0 || value["state"] != nil {
		t.Fatalf("unexpected await result: %s", result)
	}
	expectedFields := 2
	if value["warnings"] != nil {
		expectedFields++
	}
	if ok && len(value) != expectedFields {
		t.Fatalf("unexpected successful await fields: %s", result)
	}
	if !regexp.MustCompile(`"wait_time_seconds":\d+\.\d{2}[,}]`).MatchString(result) {
		t.Fatalf("elapsed seconds not encoded with two decimal places: %s", result)
	}
	return seconds
}

func TestAwaitBatchCountsAllSiblingTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool
	}{{"later_control", false}, {"earlier_control", true}, {"read", false}} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Tools = tool.NewRegistry()
			r.addAwaitTool()
			tool.Register(r.Tools, tc.name, "test sibling", nil, nil, func(struct{}) error { return nil }, func(context.Context, tool.Execution, struct{}) (any, error) {
				time.Sleep(35 * time.Millisecond)
				return nil, nil
			})
			calls := []llm.ToolCall{
				{ID: "park", Name: "await", Arguments: []byte(`{"deadline_seconds":0}`)},
				{ID: "sibling", Name: tc.name, Arguments: []byte(`{}`)},
			}
			awaitIndex := 0
			if tc.before {
				calls[0], calls[1] = calls[1], calls[0]
				awaitIndex = 1
			}
			turn, ids := batchIntents(t, r, "main", calls)
			records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := assertAwaitResult(t, string(records[awaitIndex].Result), true); elapsed < 0.03 {
				t.Fatal("sibling execution missing from elapsed time", elapsed)
			}
			var first string
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT c.name FROM tool_records r JOIN tool_calls c ON c.id=r.call_id ORDER BY r.entry_id LIMIT 1`).Scan(&first); err != nil {
				t.Fatal(err)
			}
			if first != tc.name {
				t.Fatal("await result saved before sibling", first)
			}
		})
	}
}

func TestAwaitBatchCancellationPersistsFailedResult(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	calls := []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil); done <- err }()
	waitForAwait(t, events, "main")
	cancel()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var result string
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT result_json FROM tool_calls WHERE id=?`, ids[0]).Scan(&result); err != nil {
		t.Fatal(err)
	}
	assertAwaitResult(t, result, false)
	if !strings.Contains(result, `"code":"cancelled"`) || r.Awaiting("main") {
		t.Fatal("cancelled await not settled", result)
	}
}

func TestAwaitBatchPreservesClippingWarnings(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	calls := []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{"deadline_seconds":-1}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertAwaitResult(t, string(records[0].Result), true)
	if !strings.Contains(string(records[0].Result), "input_clipped") {
		t.Fatal("clipping warning lost", string(records[0].Result))
	}
}

func TestAwaitBatchFailureDuringSiblingSettlement(t *testing.T) {
	for _, storageFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "storage_failure"}[storageFailure], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			started := make(chan struct{})
			tool.Register(r.Tools, "later_control", "test sibling", nil, nil, func(struct{}) error { return nil }, func(ctx context.Context, _ tool.Execution, _ struct{}) (any, error) {
				close(started)
				if !storageFailure {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return nil, nil
			})
			calls := []llm.ToolCall{
				{ID: "park", Name: "await", Arguments: []byte(`{"deadline_seconds":-1}`)},
				{ID: "sibling", Name: "later_control", Arguments: []byte(`{}`)},
			}
			turn, ids := batchIntents(t, r, "main", calls)
			if storageFailure {
				if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER reject_sibling_result BEFORE UPDATE OF result_json ON tool_calls WHEN OLD.provider_call_id='sibling' BEGIN SELECT RAISE(ABORT,'injected sibling storage failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil); done <- err }()
			receive(t, started)
			if !storageFailure {
				cancel()
			}
			err := receive(t, done)
			if !errors.Is(err, context.Canceled) || storageFailure && !strings.Contains(err.Error(), "injected sibling storage failure") {
				t.Fatal(err)
			}
			var result string
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT result_json FROM tool_calls WHERE id=?`, ids[0]).Scan(&result); err != nil {
				t.Fatal(err)
			}
			assertAwaitResult(t, result, false)
			if !strings.Contains(result, "input_clipped") {
				t.Fatal("cancelled await lost clipping warning", result)
			}
			var count int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM tool_records WHERE call_id=?`, ids[0]).Scan(&count); err != nil || count != 1 {
				t.Fatal("await did not settle exactly once", count, err)
			}
			if len(r.awaitRequests) != 0 || r.Awaiting("main") {
				t.Fatal("await state survived failed batch")
			}
		})
	}
}

func TestChildAwaitCancellationSettlesResultAndCleansTimer(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if step != 1 {
			t.Error("inference resumed after child cancellation")
		}
		if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "timer", Name: "wakeup_schedule", Arguments: []byte(`{"name":"owned","message":"later","delay_seconds":3600}`)}}); err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "park", Name: "await", Arguments: []byte(`{}`)}})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"background":true,"prompt":"wait","label":"cancel await"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":true,"background":true,"prompt":"wait","label":"cancel await"}`)
	actor := result["child_id"].(string)
	waitForAwait(t, events, actor)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closed, err := r.StopChild(ctx, "main", actor)
	if err != nil {
		t.Fatal(err)
	}
	requireChildLifecycle(t, closed, "cancelled", "closed")
	var saved string
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT result_json FROM tool_calls WHERE name='await' AND actor_id=?`, actor).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	assertAwaitResult(t, saved, false)
	timers := r.timers.list()
	if len(timers) != 1 || timers[0].Status != "cancelled" || r.Awaiting(actor) {
		t.Fatal("cancelled child retained await or timer", timers)
	}
}

func TestAwaitBatchPendingInputAndDuplicate(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	calls := []llm.ToolCall{
		{ID: "park", Name: "await", Arguments: []byte(`{}`)},
		{ID: "duplicate", Name: "await", Arguments: []byte(`{}`)},
	}
	turn, ids := batchIntents(t, r, "main", calls)
	if err := r.queueNotification(`{"type":"test"}`); err != nil {
		t.Fatal(err)
	}
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertAwaitResult(t, string(records[0].Result), true)
	if !strings.Contains(string(records[1].Result), "await_pending") {
		t.Fatal("duplicate registration was not rejected", string(records[1].Result))
	}
}

func TestAwaitLoadExcludesPendingExchangeAndRetainsFinalResult(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	calls := []llm.ToolCall{
		{ID: "park", Name: "await", Arguments: []byte(`{}`)},
		{ID: "sibling", Name: "wakeup_list", Arguments: []byte(`{}`)},
	}
	turn, ids := batchIntents(t, r, "main", calls)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil); done <- err }()
	waitForAwait(t, events, "main")
	loaded, err := r.Store.Load(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := r.Store.Messages(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if len(m.Calls) != 0 || m.Type == llm.TypeToolResult {
			t.Fatal("load retained part of unfinished exchange", m)
		}
	}
	var pending int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM tool_calls WHERE id=? AND result_json IS NULL`, ids[0]).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("load changed live await", pending, err)
	}
	if err := r.queueNotification(`{"type":"test"}`); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	loaded, err = r.Store.Load(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	messages, err = r.Store.Messages(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	results := 0
	for _, m := range messages {
		if m.Type == llm.TypeToolResult {
			results++
			if m.CallID == "park" {
				assertAwaitResult(t, m.Content, true)
			}
		}
	}
	if results != 2 {
		t.Fatal("load dropped completed exchange", messages)
	}
}

func TestAwaitPendingEventAndConcurrentRegistration(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(map[bool]string{true: "already_pending", false: "after_registration"}[before], func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			seedRuntime(t, r, "event registration")
			if before {
				if err := r.queueNotification(`{"type":"test"}`); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { _, err := r.awaitActor(context.Background(), "main", time.Time{}); done <- err }()
			if !before {
				waitForAwait(t, events, "main")
				if err := r.queueNotification(`{"type":"test"}`); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lost event")
			}
		})
	}
}

func TestAwaitDeadlineAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		r, events := runtimeFixture(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		deadline := time.Now().Add(time.Hour)
		if !cancelled {
			deadline = time.Now()
		}
		done := make(chan error, 1)
		go func() { _, err := r.awaitActor(ctx, "main", deadline); done <- err }()
		if cancelled {
			waitForAwait(t, events, "main")
			cancel()
		}
		select {
		case err := <-done:
			if (err != nil) != cancelled {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("await did not stop")
		}
		cancel()
	}
}

func TestAwaitCountdownStartsAtSettledBoundary(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	seedRuntime(t, r, "countdown boundary")
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "await", []byte(`{"deadline_seconds":1}`))
	if !strings.Contains(string(record.Result), `"ok":true`) {
		t.Fatal(string(record.Result))
	}
	// Simulate a foreground sibling taking longer than the requested countdown.
	time.Sleep(1100 * time.Millisecond)
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- r.settleAwait(context.Background(), "main") }()
	waitForAwait(t, events, "main")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
			t.Fatalf("sibling time consumed countdown: %s", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settled countdown did not expire")
	}
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	if len(r.notifications) != 1 || !strings.Contains(r.notifications[0].Content, "await_deadline") {
		t.Fatal("missing deadline event", r.notifications)
	}
}

func TestAwaitOmittedAndZeroDeadlineRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		args  string
		timed bool
	}{{`{}`, false}, {`{"deadline_seconds":0}`, true}} {
		t.Run(tc.args, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "deadline presence")
			record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "await", []byte(tc.args))
			if !strings.Contains(string(record.Result), `"ok":true`) {
				t.Fatal(string(record.Result))
			}
			request := r.awaitRequests["main"]
			if request.timed != tc.timed || request.duration != 0 {
				t.Fatalf("incorrect deadline presence: %+v", request)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := r.settleAwait(ctx, "main")
			if tc.timed && err != nil || !tc.timed && err != context.DeadlineExceeded {
				t.Fatalf("incorrect zero/omitted behavior: %v", err)
			}
		})
	}
}

func TestChildAwaitResumesOnParentGuidanceAndCleansTimer(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if step == 1 {
			if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "timer", Name: "wakeup_schedule", Arguments: []byte(`{"name":"owned","message":"later","delay_seconds":3600}`)}}); err != nil {
				return err
			}
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "park", Name: "await", Arguments: []byte(`{}`)}})
		}
		found := false
		foundResult := false
		for _, m := range request.Messages {
			found = found || m.Type == llm.TypeChildGuidance && strings.Contains(m.Content, "resume child")
			if m.Type == llm.TypeToolResult && m.CallID == "park" {
				assertAwaitResult(t, m.Content, true)
				foundResult = true
			}
		}
		if !found {
			t.Error("parent guidance absent after await")
		}
		if !foundResult {
			t.Error("child did not receive final await result")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Finished after guidance"})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"background":true,"prompt":"wait","label":"await child"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":true,"background":true,"prompt":"wait","label":"await child"}`)
	actor := result["child_id"].(string)
	waitForAwait(t, events, actor)
	wrong, _ := json.Marshal(map[string]any{"job_id": result["job_id"], "child_id": "main/another", "message": "must not route"})
	rejected := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main", CallID: ids[0]}, "child_message", wrong)
	if strings.Contains(string(rejected.Result), `"ok":true`) {
		t.Fatal("conflicting selector routed guidance")
	}
	args, _ := json.Marshal(map[string]any{"job_id": result["job_id"], "child_id": actor, "child_turn_id": result["child_turn_id"], "message": "resume child"})
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main", CallID: ids[0]}, "child_message", args)
	if !strings.Contains(string(record.Result), `"ok":true`) {
		t.Fatal(string(record.Result))
	}
	if _, err := r.Jobs.Wait(context.Background(), "main", result["job_id"].(string), nil); err != nil {
		t.Fatal(err)
	}
	views := r.ChildViews("main")
	if len(views) != 1 || views[0].ChildState != "idle" {
		t.Fatal(views)
	}
	timers := r.timers.list()
	if len(timers) != 1 || timers[0].Status != "cancelled" {
		t.Fatal("persistent child retained assignment timer", timers)
	}
	// Registration stays closed while the retained conversation is idle.
	record = r.Tools.Invoke(context.Background(), tool.Execution{Actor: actor}, "wakeup_schedule", []byte(`{"name":"late","message":"no","delay_seconds":3600}`))
	if strings.Contains(string(record.Result), `"ok":true`) {
		t.Fatal("idle child admitted timer")
	}
}
