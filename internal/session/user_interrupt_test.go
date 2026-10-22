package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func interruptionNotices(messages []llm.Message) int {
	count := 0
	for _, message := range messages {
		if message.Type == llm.TypeRuntimeInstruction && message.Content == prompts.SessionUserInterrupted {
			count++
		}
	}
	return count
}

func TestUserInterruptPersistsDeveloperNoticeAfterSettledCalls(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Unfinished reply"}); err != nil {
			return err
		}
		call := llm.ToolCall{ID: "never-execute", Name: "write", Arguments: []byte(`{"path":"must-not-exist","content":"bad"}`)}
		if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
			return err
		}
		r.Interrupt()
		r.Interrupt() // Repeated cancellation must not add repeated notices.
		return ctx.Err()
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Root, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("interrupted call executed", err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	if interruptionNotices(messages) != 1 || len(messages) < 3 {
		t.Fatal("missing or repeated notice", messages)
	}
	last := len(messages) - 1
	if messages[last].Role() != "developer" || messages[last-1].Type != llm.TypeToolResult || messages[last-2].Content != "Unfinished reply" {
		t.Fatal("notice did not follow the settled partial reply", messages)
	}
	var result struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(messages[last-1].Content), &result); err != nil || result.OK || result.Error.Code != "interrupted" {
		t.Fatal(messages[last-1].Content, err)
	}
	noticeEvents := 0
	for len(events) > 0 {
		event := <-events
		if event.MessageType == llm.TypeRuntimeInstruction && event.Text == prompts.SessionUserInterrupted {
			noticeEvents++
			entry, err := r.Store.Entry(event.EntryID)
			if err != nil || !entry.Visible || entry.Role != "developer" {
				t.Fatal("notice is not inspectable model input", entry, err)
			}
		}
	}
	if noticeEvents != 1 {
		t.Fatal("notice event count", noticeEvents)
	}
	// Idle cancellation creates no new notice; the next human input follows it.
	r.Interrupt()
	requests := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if interruptionNotices(req.Messages) != 1 {
			t.Fatal("next request lost or repeated interruption", req.Messages)
		}
		for i, message := range req.Messages {
			if message.Type == llm.TypeUserInput && message.Content == "Continue" && (i == 0 || req.Messages[i-1].Content != prompts.SessionUserInterrupted) {
				t.Fatal("new input did not follow interruption", req.Messages)
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Resumed"})
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue"}); err != nil || requests != 1 {
		t.Fatal("resume failed", err, requests)
	}
}

func TestNonUserCancellationDoesNotClaimUserInterruption(t *testing.T) {
	for _, kind := range []string{"provider cancellation", "deadline", "provider failure", "parent cancellation", "frontend cleanup", "completed despite interrupt"} {
		t.Run(kind, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				switch kind {
				case "provider cancellation":
					return context.Canceled
				case "deadline":
					return context.DeadlineExceeded
				case "provider failure":
					return errors.New("provider failed")
				case "parent cancellation":
					r.cancel()
					r.Interrupt() // Parent cancellation wins, as during Close.
					return ctx.Err()
				case "frontend cleanup":
					r.CancelForeground() // Cleanup can arrive before parent cancellation.
					r.Interrupt()
					return ctx.Err()
				case "completed despite interrupt":
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Complete"}); err != nil {
						return err
					}
					r.Interrupt()
					return nil
				default:
					panic("unknown case")
				}
			}}
			err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Start"})
			if (err == nil) != (kind == "completed despite interrupt") {
				t.Fatal(err)
			}
			messages, err := r.Store.Messages(r.Current())
			if err != nil || interruptionNotices(messages) != 0 {
				t.Fatal("incorrect user interruption attribution", messages, err)
			}
		})
	}
}

func TestUserInterruptAfterToolCommitPreservesSideEffects(t *testing.T) {
	r, _ := runtimeFixture(t, []llm.ScriptResponse{{Calls: []llm.ToolCall{{ID: "write", Name: "write", Arguments: []byte(`{"path":"completed.txt","content":"saved"}`)}}}})
	r.Emit = func(event Event) {
		if event.Kind == "tool" {
			r.Interrupt()
		}
	}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Write then stop"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(r.Workspace.Root, "completed.txt"))
	if err != nil || string(content) != "saved" {
		t.Fatal("interruption lost committed side effects", string(content), err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil || interruptionNotices(messages) != 1 {
		t.Fatal(messages, err)
	}
	last := len(messages) - 1
	if last < 1 || messages[last].Content != prompts.SessionUserInterrupted || messages[last-1].Type != llm.TypeToolResult || !strings.Contains(messages[last-1].Content, `"ok":true`) {
		t.Fatal("notice did not follow committed tool result", messages)
	}
}

func TestUserInterruptionPersistenceFailureIsReported(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Start")
	if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER fail_interruption BEFORE INSERT ON entries WHEN json_extract(NEW.content_json,'$.type')='runtime_instruction' BEGIN SELECT RAISE(ABORT,'forced interruption failure'); END`); err != nil {
		t.Fatal(err)
	}
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, _ func(llm.StreamEvent) error) error {
		r.Interrupt()
		return ctx.Err()
	}}
	err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Interrupt"})
	if !errors.Is(err, context.Canceled) || err == nil || !strings.Contains(err.Error(), "forced interruption failure") {
		t.Fatal("notice persistence failure was hidden", err)
	}
	if _, err := historytest.DB(t, r.Store).Exec("DROP TRIGGER fail_interruption"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error {
		requests++
		return nil
	}}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Resume"}); err == nil || !strings.Contains(err.Error(), "forced interruption failure") || requests != 0 {
		t.Fatal("inference proceeded without the missing interruption notice", err, requests)
	}
}

func TestUserInterruptionDuringAutomaticCompaction(t *testing.T) {
	for _, phase := range []string{"summary", "handoff"} {
		t.Run(phase, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Previous research notes. ", 400))
			before := r.Current()
			summaries, coding := 0, 0
			r.Emit = func(event Event) {
				if phase == "handoff" && event.Kind == "continuation" {
					r.Interrupt()
				}
			}
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if !req.NoTools {
					coding++
					return nil
				}
				summaries++
				if phase == "summary" {
					r.Interrupt()
					return ctx.Err()
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Earlier research summarized."})
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Continue research"}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if summaries != 1 || coding != 0 || (r.Current() != before) != (phase == "handoff") {
				t.Fatal("wrong compaction boundary", summaries, coding, before, r.Current())
			}
			messages, err := r.Store.Messages(r.Current())
			if err != nil || interruptionNotices(messages) != 1 || messages[len(messages)-1].Role() != "developer" {
				t.Fatal("notice missing from active writable session", messages, err)
			}
			var count int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries e JOIN turns t ON t.id=e.turn_id WHERE e.session_id=? AND e.model_visible=1 AND json_extract(e.content_json,'$.content')=? AND t.session_id=? AND t.status='interrupted'`, r.Current(), prompts.SessionUserInterrupted, r.Current()).Scan(&count); err != nil || count != 1 {
				t.Fatal("notice is not owned by the active interrupted turn", count, err)
			}
		})
	}
}
