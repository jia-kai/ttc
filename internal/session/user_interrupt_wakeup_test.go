package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"ttc/internal/capture"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

func userInterruptNotice(t *testing.T, r *Runtime, want int) history.Entry {
	t.Helper()
	entries, err := r.Store.Branch(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var notice history.Entry
	count := 0
	for _, entry := range entries {
		if entry.Kind != "message" {
			continue
		}
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			t.Fatal(err)
		}
		if message.Content != prompts.SessionUserInterrupted {
			continue
		}
		count++
		notice = entry
		if message.Type != llm.TypeRuntimeInstruction || message.Role() != "developer" || entry.Role != "developer" || entry.Actor != "main" || !entry.Visible || entry.TurnID == "" {
			t.Fatalf("interrupt notice lost canonical identity or visibility: %+v / %+v", entry, message)
		}
	}
	if count != want {
		t.Fatalf("persisted interrupt notices = %d, want %d", count, want)
	}
	return notice
}

func runUserInterruptedTurn(t *testing.T, r *Runtime) history.Entry {
	t.Helper()
	started := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Partial work before the user interrupted."}); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { done <- r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start the work"}) }()
	receive(t, started)
	r.Interrupt()
	r.Interrupt()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted turn returned %v", err)
	}
	notice := userInterruptNotice(t, r, 1)
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) == 0 || messages[len(messages)-1].Content != prompts.SessionUserInterrupted {
		t.Fatal("interrupt notice was not the last model-visible message of the turn", messages)
	}
	var status string
	if err := historytest.DB(t, r.Store).QueryRow("SELECT status FROM turns WHERE id=?", notice.TurnID).Scan(&status); err != nil || status != "interrupted" {
		t.Fatal("interrupt notice was not attached to the interrupted turn", status, err)
	}
	// The persisted instruction is history, not another source of automatic wakeups.
	if r.HasNotifications() {
		t.Fatal("interrupt notice queued a wakeup")
	}
	r.Interrupt()
	if got := userInterruptNotice(t, r, 1); got.ID != notice.ID {
		t.Fatal("idle interrupt replaced the turn's notice")
	}
	return notice
}

func TestUserInterruptNoticePrecedesNextNotificationRun(t *testing.T) {
	for _, source := range []string{"job", "child", "timer"} {
		t.Run(source, func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			notice := runUserInterruptedTurn(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var kind, key, id string
			switch source {
			case "job":
				kind, key = "job_exit", "job_id"
				var err error
				id, err = r.Jobs.Start("main", "printf 'completed after interruption\\n'", r.Workspace.Root, time.Second, true, true, capture.OutputOptions{}, "interrupt completion")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Jobs.Wait(ctx, "main", id, nil); err != nil {
					t.Fatal(err)
				}
				// Process exit precedes the completion callback; join its publication
				// through the emitted event rather than racing notification admission.
				for receive(t, events).Kind != "job" {
				}
			case "child":
				kind, key = "child_turn_finished", "child_id"
				r.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Child completed after interruption."}}}
				calls := []llm.ToolCall{{ID: "spawn-after-interrupt", Name: "subagent", Arguments: []byte(`{"persistent":false,"background":true,"prompt":"Finish the independent work","label":"interrupt completion"}`)}}
				turn, ids := batchIntents(t, r, "main", calls)
				records, err := r.runToolBatch(ctx, turn, "main", r.Tools, calls, ids, nil)
				if err != nil || len(records) != 1 {
					t.Fatal("launch child", records, err)
				}
				var result struct {
					OK      bool   `json:"ok"`
					ChildID string `json:"child_id"`
					JobID   string `json:"job_id"`
				}
				if err := json.Unmarshal(records[0].Result, &result); err != nil || !result.OK || result.ChildID == "" || result.JobID == "" {
					t.Fatal("invalid child launch", string(records[0].Result), err)
				}
				id = result.ChildID
				if err := r.Store.FinishTurn(turn, "completed"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Jobs.Wait(ctx, "main", result.JobID, nil); err != nil {
					t.Fatal(err)
				}
			case "timer":
				kind, key = "wakeup", "wakeup_id"
				wake, err := r.timers.schedule(wakeupSchedule{Name: "interrupt completion", Message: "Check independent work"}, "main", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				id = wake.ID
				receive(t, wake.done)
			}
			if !r.HasNotifications() {
				t.Fatal("independent completion did not queue a notification")
			}
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				instruction, notification, count := -1, -1, 0
				for i, message := range req.Messages {
					if message.Content == prompts.SessionUserInterrupted {
						count++
						instruction = i
						if message.Type != llm.TypeRuntimeInstruction || message.Role() != "developer" {
							t.Error("interrupt notice was not projected as a developer instruction", message)
						}
					}
					if message.Type != llm.TypeRuntimeNotification {
						continue
					}
					var body map[string]json.RawMessage
					if err := json.Unmarshal([]byte(message.Content), &body); err != nil {
						t.Fatal(err)
					}
					var gotKind, gotID string
					if err := json.Unmarshal(body["type"], &gotKind); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(body[key], &gotID); err != nil {
						t.Fatal(err)
					}
					if gotKind == kind && gotID == id {
						notification = i
					}
				}
				if count != 1 || instruction < 0 || notification <= instruction {
					t.Errorf("notice must precede %s delivery: count=%d instruction=%d notification=%d", kind, count, instruction, notification)
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Observed independent completion."})
			}}
			if err := r.Run(nil); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || r.HasNotifications() {
				t.Fatal("notification turn did not drain exactly one request", requests, r.HasNotifications())
			}
			if got := userInterruptNotice(t, r, 1); got.ID != notice.ID {
				t.Fatal("notification admission duplicated or replaced the interruption notice")
			}
			if source == "timer" {
				listed := r.timers.list()
				if len(listed) != 1 || listed[0].Last != "delivered" {
					t.Fatal("timer notification was not acknowledged", listed)
				}
			}
		})
	}
}

func TestUserInterruptNoticePrecedesLaterUserInput(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	notice := runUserInterruptedTurn(t, r)
	requests := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		instruction, input := -1, -1
		for i, message := range req.Messages {
			if message.Type == llm.TypeRuntimeInstruction && message.Content == prompts.SessionUserInterrupted {
				instruction = i
			}
			if message.Type == llm.TypeUserInput && message.Content == "A different task" {
				input = i
			}
		}
		if instruction < 0 || input <= instruction {
			t.Error("later user input preceded the interruption notice", instruction, input)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Handling the new task."})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "A different task"}); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || userInterruptNotice(t, r, 1).ID != notice.ID {
		t.Fatal("later user turn duplicated the interruption notice", requests)
	}
}

func TestUserInterruptNoticeWaitsForToolSettlement(t *testing.T) {
	r, _ := runtimeFixture(t, []llm.ScriptResponse{{Calls: []llm.ToolCall{{ID: "blocked-read", Name: "read", Arguments: []byte(`{}`)}}}})
	r.Tools = tool.NewRegistry()
	started, cancelled, settle := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(settle)
	tool.Register(r.Tools, "read", "gated cancellation fixture", nil, nil, func(struct{}) error { return nil }, func(ctx context.Context, _ tool.Execution, _ struct{}) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-settle
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Read before proceeding"}) }()
	receive(t, started)
	r.Interrupt()
	r.Interrupt()
	receive(t, cancelled)
	// A completion can arrive while cancellation is still joining foreground
	// work. It must remain queued until after the interruption notice is saved.
	if err := r.queueNotification(`{"type":"job_exit","job_id":"during-settlement"}`); err != nil {
		t.Fatal(err)
	}
	userInterruptNotice(t, r, 0)
	select {
	case err := <-done:
		t.Fatal("turn returned before its tool settled", err)
	default:
	}
	settle <- struct{}{}
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	notice := userInterruptNotice(t, r, 1)
	var resultEntry int64
	var result string
	if err := historytest.DB(t, r.Store).QueryRow("SELECT tr.entry_id,tc.result_json FROM tool_calls tc JOIN tool_records tr ON tr.call_id=tc.id WHERE tc.provider_call_id='blocked-read'").Scan(&resultEntry, &result); err != nil {
		t.Fatal(err)
	}
	if resultEntry == 0 || resultEntry >= notice.ID || !json.Valid([]byte(result)) {
		t.Fatal("notice preceded durable tool settlement", resultEntry, notice.ID, result)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil || len(messages) < 2 || messages[len(messages)-2].Type != llm.TypeToolResult || messages[len(messages)-1].Content != prompts.SessionUserInterrupted {
		t.Fatal("tool result and interruption notice are out of order", messages, err)
	}
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		instruction, completion := -1, -1
		for i, message := range req.Messages {
			if message.Content == prompts.SessionUserInterrupted {
				instruction = i
			}
			if message.Type == llm.TypeRuntimeNotification {
				completion = i
			}
		}
		if instruction < 0 || completion <= instruction {
			t.Fatal("completion overtook interruption notice", instruction, completion)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Observed the queued completion."})
	}}
	if err := r.Run(nil); err != nil {
		t.Fatal(err)
	}
}

func TestUserInterruptNoticeDoesNotFollowOtherCancellation(t *testing.T) {
	for _, source := range []string{"provider", "parent_context"} {
		t.Run(source, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Partial response without a user interrupt."}); err != nil {
					return err
				}
				if source == "parent_context" {
					r.cancel() // Cancel the runtime context, which parents the admitted turn.
					<-ctx.Done()
					return ctx.Err()
				}
				return context.Canceled
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start work"}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			userInterruptNotice(t, r, 0)
			if r.HasNotifications() {
				t.Fatal("non-user cancellation queued an interruption wakeup")
			}
		})
	}
}
