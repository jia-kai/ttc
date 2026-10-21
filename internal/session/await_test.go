package session

import (
	"context"
	"encoding/json"
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

func TestAwaitSettlesToolBeforeParkingAndUserInputResumes(t *testing.T) {
	r, events := runtimeFixture(t, []llm.ScriptResponse{
		{Calls: []llm.ToolCall{{ID: "park", Name: "await", Arguments: []byte(`{}`)}}},
		{Text: "Resumed answer"},
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "wait for input"}) }()
	waitForAwait(t, events, "main")
	var result string
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT result_json FROM tool_calls WHERE name='await'`).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"state":"awaiting"`) || !strings.Contains(result, `"ok":true`) {
		t.Fatal("await tool did not finish before parking", result)
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
		for _, m := range request.Messages {
			found = found || m.Type == llm.TypeChildGuidance && strings.Contains(m.Content, "resume child")
		}
		if !found {
			t.Error("parent guidance absent after await")
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
