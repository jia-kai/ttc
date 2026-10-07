package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

func partialRetryFailure(req llm.Request) *llm.PartialError {
	return &llm.PartialError{
		Err:   errors.New("synthetic stream interruption"),
		Retry: llm.Retry{Attempt: req.PriorAttempts + 2, DelayMilliseconds: 0, Reason: "stream interrupted", MaxAttempts: llm.DefaultMaxAttempts},
	}
}

func partialRetryText(emit func(llm.StreamEvent) error, text string) error {
	if err := emit(llm.StreamEvent{Kind: "text", Text: text}); err != nil {
		return err
	}
	// An interrupted response must retain canonical text, not native replay.
	return emit(llm.StreamEvent{Kind: "state", StateVersion: 1, StateItem: json.RawMessage(`{"type":"message","id":"partial-native"}`)})
}

func partialRetryWarning(t *testing.T, req llm.Request) llm.Message {
	t.Helper()
	var found llm.Message
	for _, m := range req.Messages {
		if m.Content == prompts.Recovery {
			if m.Role != "developer" || !m.Runtime || m.RequestID == 0 || m.InputSource != "" || m.InputTimeMS != 0 {
				t.Errorf("recovery warning has incorrect provenance: %#v", m)
			}
			found = m
		}
	}
	if found.Content == "" {
		t.Error("recovery request omitted exact developer warning")
	}
	return found
}

func partialRetryMessage(t *testing.T, req llm.Request, role, content string) llm.Message {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == role && m.Content == content {
			return m
		}
	}
	t.Errorf("request omitted %s message %q", role, content)
	return llm.Message{}
}

func partialRetryResult(t *testing.T, req llm.Request, callID string, ok bool, code string) {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role != "tool" || m.CallID != callID {
			continue
		}
		var result struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(m.Content), &result); err != nil || result.OK != ok || result.Error.Code != code {
			t.Errorf("incorrect result for %s: %s (%v)", callID, m.Content, err)
		}
		return
	}
	t.Errorf("missing settled result for %s", callID)
}

// Direct child runs exercise the shared child/aside loop and allow injection of
// a failing stdout writer instead of the job supervisor's buffered writer.
func partialRetryRunActor(t *testing.T, r *Runtime, actor string, stdout io.Writer) error {
	t.Helper()
	if actor == "main" {
		return r.Run(&llm.Message{Role: "user", Content: "Recover this task"})
	}
	seedRuntime(t, r, "Private main context")
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	err = r.runChild(context.Background(), childTask{actor: actor, turn: turn, prompt: "Isolated task", selection: r.CurrentSelection(), tools: r.Tools}, stdout, io.Discard)
	status := "completed"
	if err != nil {
		status = "failed"
	}
	if finishErr := r.Store.FinishTurn(turn, status); finishErr != nil {
		t.Fatal(finishErr)
	}
	return err
}

func partialRetryNoMainLeak(t *testing.T, r *Runtime) {
	t.Helper()
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Content == prompts.Recovery || m.Content == "Isolated partial evidence" || m.Content == btwInstruction {
			t.Errorf("isolated instructions/output leaked into main context: %#v", m)
		}
	}
}

func TestPartialRetryMainPreservesExecutedToolsAndInterruptsLaterCalls(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	var executions atomic.Int32
	tool.Register(r.Tools, "partial_counter", "Count executions", map[string]any{}, nil, func(struct{}) error { return nil }, func(context.Context, tool.Execution, struct{}) (any, error) {
		return map[string]any{"ok": true, "executions": executions.Add(1)}, nil
	})
	step := 0
	var warning llm.Message
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		switch step {
		case 1:
			if req.PriorAttempts != 0 {
				t.Errorf("initial prior attempts = %d", req.PriorAttempts)
			}
			call := llm.ToolCall{ID: "earlier", Name: "partial_counter", Arguments: []byte(`{}`)}
			return emit(llm.StreamEvent{Kind: "call", Call: &call})
		case 2:
			if req.PriorAttempts != 0 || executions.Load() != 1 {
				t.Error("successful tool response did not reset attempt accounting")
			}
			partialRetryResult(t, req, "earlier", true, "")
			if err := partialRetryText(emit, "Partial main evidence"); err != nil {
				return err
			}
			call := llm.ToolCall{ID: "interrupted", Name: "partial_counter", Arguments: []byte(`{}`)}
			if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
				return err
			}
			return partialRetryFailure(req)
		case 3:
			if req.PriorAttempts != 1 || executions.Load() != 1 {
				t.Error("partial stream executed a tool or lost attempts")
			}
			partialRetryResult(t, req, "earlier", true, "")
			partialRetryResult(t, req, "interrupted", false, "interrupted")
			m := partialRetryMessage(t, req, "assistant", "Partial main evidence")
			if m.State != nil || len(m.Calls) != 1 || m.Calls[0].ID != "interrupted" {
				t.Errorf("partial history is not canonical: %#v", m)
			}
			warning = partialRetryWarning(t, req)
			if warning.RequestID != m.RequestID {
				t.Error("warning not linked to failed producing request")
			}
			return emit(llm.StreamEvent{Kind: "text", Text: "Recovered final answer"})
		default:
			return errors.New("unexpected extra request")
		}
	}}
	if err := r.Run(&llm.Message{Role: "user", Content: "Execute once, then recover"}); err != nil {
		t.Fatal(err)
	}
	if step != 3 || executions.Load() != 1 {
		t.Fatal(step, executions.Load())
	}
	var turns, distinctTurns, completed, failedRequests int
	if err := r.Store.DB.QueryRow("SELECT count(*),sum(status='completed') FROM turns WHERE actor_id='main'").Scan(&turns, &completed); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(DISTINCT turn_id),sum(status='failed') FROM model_requests WHERE purpose='coding'").Scan(&distinctTurns, &failedRequests); err != nil {
		t.Fatal(err)
	}
	if turns != 1 || completed != 1 || distinctTurns != 1 || failedRequests != 1 {
		t.Fatal("recovery did not complete original turn", turns, completed, distinctTurns, failedRequests)
	}
	var recorded llm.Message
	var raw string
	if err := r.Store.DB.QueryRow("SELECT content_json FROM entries WHERE role='developer' AND json_extract(content_json,'$.content')=?", prompts.Recovery).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil || !reflect.DeepEqual(recorded, warning) {
		t.Fatal("warning was not durably recorded", raw, err)
	}
}

func TestPartialRetryAttemptsContinueAndSuccessfulToolResponseResets(t *testing.T) {
	for _, actor := range []string{"main", "main/child_attempts"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			want := []int{0, 1, 2, 0, 1}
			step := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if step >= len(want) {
					return errors.New("unexpected extra request")
				}
				if req.PriorAttempts != want[step] {
					t.Errorf("request %d prior attempts = %d, want %d", step, req.PriorAttempts, want[step])
				}
				step++
				if step == 3 {
					call := llm.ToolCall{ID: "successful", Name: "glob", Arguments: []byte(`{"pattern":"*.txt"}`)}
					return emit(llm.StreamEvent{Kind: "call", Call: &call})
				}
				if step == 5 {
					partialRetryWarning(t, req)
					partialRetryResult(t, req, "successful", true, "")
					return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
				}
				if err := partialRetryText(emit, fmt.Sprintf("Partial attempt %d", step)); err != nil {
					return err
				}
				return partialRetryFailure(req)
			}}
			if err := partialRetryRunActor(t, r, actor, io.Discard); err != nil {
				t.Fatal(err)
			}
			if step != len(want) {
				t.Fatal("missing continuation", step)
			}
		})
	}
}

func TestPartialRetryChildIsolationRetainedAndClosedContexts(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			seedRuntime(t, r, "Private main context")
			step := 0
			var actor string
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				step++
				for _, m := range req.Messages {
					if m.Content == "Private main context" {
						t.Error("child inherited main transcript")
					}
				}
				if req.System != childSystemTemplate {
					t.Error("child missing child-specific system prompt")
				}
				switch step {
				case 1:
					actor = req.ConversationID
					if req.PriorAttempts != 0 {
						t.Error("child started with prior attempts")
					}
					if err := partialRetryText(emit, "Isolated partial evidence"); err != nil {
						return err
					}
					call := llm.ToolCall{ID: "child-interrupted", Name: "write", Arguments: []byte(`{"path":"must-not-exist.txt","content":"bad"}`)}
					if err := emit(llm.StreamEvent{Kind: "call", Call: &call}); err != nil {
						return err
					}
					return partialRetryFailure(req)
				case 2:
					if req.ConversationID != actor || req.PriorAttempts != 1 {
						t.Error("child continuation lost identity/attempts")
					}
					partialRetryWarning(t, req)
					if m := partialRetryMessage(t, req, "assistant", "Isolated partial evidence"); m.State != nil {
						t.Error("child partial retained replay state")
					}
					partialRetryResult(t, req, "child-interrupted", false, "interrupted")
					return emit(llm.StreamEvent{Kind: "text", Text: "Recovered child answer"})
				case 3:
					if !persistent || req.ConversationID != actor || req.PriorAttempts != 0 {
						t.Error("retained follow-up lost identity/reset")
					}
					partialRetryMessage(t, req, "assistant", "Recovered child answer")
					return emit(llm.StreamEvent{Kind: "text", Text: "Follow-up answer"})
				default:
					return errors.New("unexpected child request")
				}
			}}
			args := fmt.Sprintf(`{"persistent":%t,"prompt":"Isolated task","label":"recovery child"}`, persistent)
			turn, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
			result := childInvocation(t, r, ids[0], args)
			if result["status"] != "completed" || result["answer"] != "Recovered child answer" || step != 2 {
				t.Fatal(result, step)
			}
			partialRetryNoMainLeak(t, r)
			followup, _ := json.Marshal(map[string]any{"persistent": persistent, "child_id": result["child_id"], "prompt": "Follow up"})
			second := childInvocation(t, r, ids[0], string(followup))
			if persistent {
				if second["status"] != "completed" || step != 3 || len(r.ChildViews("main")) != 1 {
					t.Fatal(second, step, r.ChildViews("main"))
				}
			} else if second["ok"] == true || step != 2 || len(r.ChildViews("main")) != 0 {
				t.Fatal("closed child accepted follow-up", second, step)
			}
			var writes int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM file_changes").Scan(&writes); err != nil || writes != 0 {
				t.Fatal("interrupted child write executed", writes, err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPartialRetryAsideRecoversWithoutLeakingOrPublishingPartialAnswer(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "Private main context")
	step := 0
	var actor string
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		partialRetryMessage(t, req, "user", "Private main context")
		partialRetryMessage(t, req, "developer", btwInstruction)
		if req.System != systemTemplate {
			t.Error("aside changed shared system instructions")
		}
		if step == 1 {
			actor = req.ConversationID
			if req.PriorAttempts != 0 {
				t.Error("aside started with prior attempts")
			}
			if err := partialRetryText(emit, "Isolated partial evidence"); err != nil {
				return err
			}
			return partialRetryFailure(req)
		}
		if step != 2 {
			return errors.New("unexpected aside request")
		}
		if req.ConversationID != actor || req.PriorAttempts != 1 {
			t.Error("aside lost identity/attempts")
		}
		partialRetryWarning(t, req)
		if m := partialRetryMessage(t, req, "assistant", "Isolated partial evidence"); m.State != nil {
			t.Error("aside partial retained replay state")
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Recovered aside answer"})
	}}
	id, err := r.StartBTW("Answer privately")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := r.Jobs.Wait(ctx, "main", id, nil)
	if err != nil || result.Status != "completed" || result.Stdout != "Recovered aside answer" || step != 2 {
		t.Fatal(result, err, step)
	}
	partialRetryNoMainLeak(t, r)
	if r.HasNotifications() {
		t.Fatal("aside recovery steered main")
	}
}

func TestPartialRetryCallStartDoesNotInventToolOutcome(t *testing.T) {
	for _, actor := range []string{"main", "main/child_announcement"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			step := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				step++
				if step == 1 {
					if err := emit(llm.StreamEvent{Kind: "call_start", CallStart: &llm.ToolStart{ID: "announced-only", Name: "write"}}); err != nil {
						return err
					}
					return partialRetryFailure(req)
				}
				if step != 2 {
					return errors.New("unexpected extra request")
				}
				partialRetryWarning(t, req)
				for _, m := range req.Messages {
					if m.Role == "tool" || len(m.Calls) != 0 {
						t.Error("announcement invented executable call or outcome", m)
					}
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
			}}
			if err := partialRetryRunActor(t, r, actor, io.Discard); err != nil {
				t.Fatal(err)
			}
			var calls, outcomes int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM tool_calls").Scan(&calls); err != nil {
				t.Fatal(err)
			}
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE kind='tool_result'").Scan(&outcomes); err != nil {
				t.Fatal(err)
			}
			if step != 2 || calls != 0 || outcomes != 0 {
				t.Fatal(step, calls, outcomes)
			}
		})
	}
}

func TestPartialRetryCancellationDuringWaitStopsNewRequest(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	waiting := make(chan struct{}, 1)
	r.Emit = func(e Event) {
		if e.Retry != nil {
			waiting <- struct{}{}
		}
	}
	var requests atomic.Int32
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests.Add(1)
		if err := partialRetryText(emit, "Partial before cancellation"); err != nil {
			return err
		}
		failure := partialRetryFailure(req)
		failure.Retry.DelayMilliseconds = 60_000
		return failure
	}}
	done := make(chan error, 1)
	go func() { done <- r.Run(&llm.Message{Role: "user", Content: "Cancel recovery wait"}) }()
	receive(t, waiting)
	r.Interrupt()
	if err := receive(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("cancellation submitted a new request", requests.Load())
	}
	var status string
	if err := r.Store.DB.QueryRow("SELECT status FROM turns WHERE actor_id='main'").Scan(&status); err != nil || status != "interrupted" {
		t.Fatal(status, err)
	}
}

func TestPartialRetryUnclassifiedFailuresRemainFinal(t *testing.T) {
	permanent := errors.New("permanent provider failure")
	for _, failure := range []error{permanent, &llm.TransientError{Err: errors.New("unclassified transient failure")}} {
		for _, actor := range []string{"main", "main/child_final"} {
			t.Run(actor+"/"+failure.Error(), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				requests := 0
				r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					requests++
					if err := partialRetryText(emit, "Final partial evidence"); err != nil {
						return err
					}
					return failure
				}}
				if err := partialRetryRunActor(t, r, actor, io.Discard); !errors.Is(err, failure) {
					t.Fatal("lost original final error", err)
				}
				var warnings int
				if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.content')=?", prompts.Recovery).Scan(&warnings); err != nil {
					t.Fatal(err)
				}
				if requests != 1 || warnings != 0 {
					t.Fatal("final failure retried", requests, warnings)
				}
			})
		}
	}
}

type partialRetryFailWriter struct{ err error }

func (w partialRetryFailWriter) Write([]byte) (int, error) { return 0, w.err }

func TestPartialRetryCallbackErrorsCannotAuthorizeRecovery(t *testing.T) {
	for _, mode := range []string{"main nil call", "child nil call", "child stdout"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			requests := 0
			var callbackErr error
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				if strings.HasSuffix(mode, "stdout") {
					callbackErr = emit(llm.StreamEvent{Kind: "text", Text: "Writer must reject this"})
				} else {
					callbackErr = emit(llm.StreamEvent{Kind: "call"})
				}
				if callbackErr == nil {
					return errors.New("expected callback failure")
				}
				failure := partialRetryFailure(req)
				failure.Err = callbackErr // Even a wrongly wrapping provider must not authorize recovery.
				return failure
			}}
			actor, stdout := "main", io.Writer(io.Discard)
			if strings.HasPrefix(mode, "child") {
				actor = "main/child_callback"
			}
			if strings.HasSuffix(mode, "stdout") {
				stdout = partialRetryFailWriter{errors.New("stdout failed")}
			}
			err := partialRetryRunActor(t, r, actor, stdout)
			if callbackErr == nil || !errors.Is(err, callbackErr) {
				t.Fatal("callback error lost", callbackErr, err)
			}
			var warnings int
			if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.content')=?", prompts.Recovery).Scan(&warnings); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || warnings != 0 {
				t.Fatal("callback failure recovered", requests, warnings)
			}
		})
	}
}

func TestPartialRetryMalformedMetadataIsFinal(t *testing.T) {
	cases := map[string]llm.Retry{
		"attempt":           {Attempt: 1, MaxAttempts: llm.DefaultMaxAttempts, Reason: "stream interrupted"},
		"zero maximum":      {Attempt: 2, Reason: "stream interrupted"},
		"negative maximum":  {Attempt: 2, MaxAttempts: -1, Reason: "stream interrupted"},
		"exhausted maximum": {Attempt: 3, MaxAttempts: 2, Reason: "stream interrupted"},
		"negative delay":    {Attempt: 2, MaxAttempts: llm.DefaultMaxAttempts, DelayMilliseconds: -1, Reason: "stream interrupted"},
		"missing reason":    {Attempt: 2, MaxAttempts: llm.DefaultMaxAttempts},
	}
	for name, retry := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			requests := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				requests++
				if err := partialRetryText(emit, "Malformed recovery metadata"); err != nil {
					return err
				}
				failure := partialRetryFailure(req)
				failure.Retry = retry
				return failure
			}}
			if err := r.Run(&llm.Message{Role: "user", Content: "Reject malformed retry"}); err == nil {
				t.Fatal("malformed retry succeeded")
			}
			if requests != 1 {
				t.Fatal("malformed metadata retried", requests)
			}
		})
	}
}

func TestPartialRetryCompactionPreservesExactDeveloperWarning(t *testing.T) {
	for _, actor := range []string{"main", "main/child_compaction"} {
		t.Run(actor, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			coding, summaries := 0, 0
			var failedRequest int64
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries++
					return emit(llm.StreamEvent{Kind: "text", Text: "Earlier partial research was interrupted; continue the original task."})
				}
				coding++
				if coding == 1 {
					if req.PriorAttempts != 0 {
						t.Error("initial compaction test request has prior attempts")
					}
					if err := r.Store.DB.QueryRow("SELECT max(id) FROM model_requests WHERE actor_id=? AND purpose='coding'", actor).Scan(&failedRequest); err != nil {
						return err
					}
					if err := partialRetryText(emit, strings.Repeat("Oversized partial research output. ", 400)); err != nil {
						return err
					}
					return partialRetryFailure(req)
				}
				if coding != 2 {
					return errors.New("unexpected coding request after compaction")
				}
				warning := partialRetryWarning(t, req)
				if summaries == 0 || warning.RequestID != failedRequest || req.PriorAttempts != 1 {
					t.Error("compaction lost pending recovery identity/attempts", summaries, warning, req.PriorAttempts)
				}
				if !contextbuild.Fits(req.Selection, req.System, req.Tools, req.Messages, false) {
					t.Error("oversized recovery request bypassed admission")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Recovered after compaction"})
			}}
			if err := partialRetryRunActor(t, r, actor, io.Discard); err != nil {
				t.Fatal(err)
			}
			if coding != 2 || summaries != 1 {
				t.Fatal("expected one compaction before recovery", coding, summaries)
			}
			if actor != "main" {
				partialRetryNoMainLeak(t, r)
			}
		})
	}
}
