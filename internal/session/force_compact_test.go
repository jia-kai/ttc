package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/capture"
	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func forceCompactionFixture(t *testing.T) *Runtime {
	t.Helper()
	r, _ := runtimeFixture(t, nil)
	compactionBudget(t, r)
	seedCompactionHistory(t, r, strings.Repeat("Locally overestimated original evidence. ", 10000))
	return r
}

func appendForceMessages(t *testing.T, r *Runtime, messages ...llm.Message) {
	t.Helper()
	for _, message := range messages {
		if _, err := r.Store.Append(r.Current(), "", "main", "message", message.Role(), true, message); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForceCompactCommandPreservesStructuredRequestAndPendingChildAnswer(t *testing.T) {
	r := forceCompactionFixture(t)
	selection, source := r.CurrentSelection(), r.Current()
	replay := &llm.ReplayState{Provider: selection.Provider, Model: selection.Model.RequestID(), Version: 1, Items: []json.RawMessage{json.RawMessage(`{"native":"original reasoning"}`)}}
	appendForceMessages(t, r,
		llm.Message{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "old", Name: "read", Arguments: json.RawMessage(`{"path":"original.txt"}`)}}, State: replay},
		llm.Message{Type: llm.TypeToolResult, CallID: "old", Content: "Original structured evidence."},
		llm.Message{Type: llm.TypeAssistant, Content: strings.Repeat("Completed analysis. ", 200)},
	)
	fresh := []llm.Message{
		{Type: llm.TypeAssistant, Calls: []llm.ToolCall{{ID: "fresh", Name: "read", Arguments: json.RawMessage(`{"path":"fresh.txt"}`)}}, State: replay},
		{Type: llm.TypeToolResult, CallID: "fresh", Content: "FRESH_UNCONSUMED_RESULT"},
	}
	appendForceMessages(t, r, fresh...)
	original, err := r.Store.Messages(source)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareCompaction(original, selection)
	if err != nil || prepared.retention.Start != len(original)-len(fresh) {
		t.Fatal("fixture must retain the fresh complete tool cycle", prepared.retention, err)
	}
	const focus = "Preserve exact measurements and unresolved checks."
	calls := 0
	var finish int64
	var notice llm.Message
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		calls++
		want := append(append([]llm.Message(nil), prepared.prefix...), llm.Message{Type: llm.TypeRuntimeInstruction, Content: fmt.Sprintf(prompts.CompactionFocus, focus)}, compactionInstruction())
		if req.ConversationID != source || req.System != systemTemplate || !reflect.DeepEqual(req.Selection, selection) || !reflect.DeepEqual(req.Tools, r.Tools.Definitions()) || !reflect.DeepEqual(req.Messages, want) {
			t.Fatal("force changed coding cache identity or structured prefix/focus/directive")
		}
		if !req.NoTools || req.OutputTokens != selection.Model.Budget.SummaryOutputAllowance || compactionMessagesContain(req.Messages, "FRESH_UNCONSUMED_RESULT") {
			t.Fatal("force bypassed no-tools/output limits or consumed fresh results")
		}
		actor := "main/child_force_finished"
		turn, err := r.Store.BeginChildTurn(source, actor, selection)
		if err != nil {
			return err
		}
		result, err := r.Store.Append(source, turn, actor, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: "Measurements verified."})
		if err != nil {
			return err
		}
		completion := history.ChildFinish{ChildID: actor, TurnID: turn, JobID: "job_force_finished", Outcome: "completed", ContextState: "closed", ResultEntry: result, Answer: "Measurements verified."}
		finish, err = r.Store.FinishChildTurn(source, completion)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(childCompletionNotification(completion))
		r.orderMu.Lock()
		err = r.queueCommittedNotificationLocked(string(body), finish)
		if err == nil {
			notice = r.notifications[len(r.notifications)-1]
		}
		r.orderMu.Unlock()
		if err != nil {
			return err
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Evidence summarized; inspect fresh measurements next."})
	}}
	if _, err := r.Command("/compact " + focus); err == nil || !recoverableCompaction(err) || calls != 0 || r.Current() != source {
		t.Fatal("normal compaction must refuse the local overestimate before calling provider", calls, err)
	}
	var requests int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM model_requests WHERE purpose='compaction'").Scan(&requests); err != nil || requests != 0 {
		t.Fatal("normal preflight refusal created a request", requests, err)
	}
	if _, err := r.Command("/force-compact " + focus); err != nil || calls != 1 || r.Current() == source || r.checkContext(false) != nil {
		t.Fatal("forced compaction did not commit a usable fitting handoff", calls, err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	wantContext := llm.NewCompactionContext("Evidence summarized; inspect fresh measurements next.")
	if len(messages) == 0 || !reflect.DeepEqual(messages[0], wantContext) || messages[0].Role() != "developer" {
		t.Fatal("force did not persist the same typed context used by normal compaction", messages)
	}
	for _, message := range messages {
		if message.Type == llm.TypeAssistant && (message.Content == "Evidence summarized; inspect fresh measurements next." || message.Content == wantContext.Content) {
			t.Fatal("force persisted a generated assistant handoff artifact")
		}
		if message.State != nil || strings.Contains(message.Content, focus) || message.Content == prompts.Compaction {
			t.Fatal("handoff leaked replay or temporary summary instructions")
		}
	}
	canonicalFresh := canonicalCompaction(fresh)
	if len(messages) < len(fresh) || !reflect.DeepEqual(messages[len(messages)-len(fresh):], canonicalFresh) {
		t.Fatal("force lost or changed the fresh tool cycle")
	}
	if len(r.notifications) != 1 || !reflect.DeepEqual(r.notifications[0], notice) || notice.EventSeq != finish {
		t.Fatal("force consumed or changed a child completion queued during summary")
	}
	cm, _, err := r.runtimeContext(context.Background(), "main", selection, contextCursor{})
	if err != nil || !compactionFits(selection, systemTemplate, r.Tools.Definitions(), messages, r.notifications, cm) {
		t.Fatal("successful force handoff does not fit with pending completion/runtime", err)
	}
	admitted, _, err := r.admitMain(context.Background(), admissionTurn(t, r), selection)
	if err != nil {
		t.Fatal("next coding request could not admit the handoff", err)
	}
	found := 0
	for _, message := range admitted.Messages {
		if message.EventSeq == finish && message.Content == notice.Content {
			found++
		}
	}
	if found != 1 || r.HasNotifications() {
		t.Fatal("pending child answer was not delivered exactly once", found)
	}
	if err := r.Store.FinishRequest(admitted.RequestID, "completed", nil); err != nil {
		t.Fatal(err)
	}
}

func TestForceCompactPreservesRequiredRecoveryWarning(t *testing.T) {
	r := forceCompactionFixture(t)
	request, err := r.Store.StartRequest(r.Current(), "", "main", "coding", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	warning := llm.Message{Type: llm.TypeRuntimeInstruction, Content: prompts.Recovery, RequestID: request}
	if _, err := r.ensureRecovery("", "main", warning); err != nil {
		t.Fatal(err)
	}
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: "Earlier partial evidence summarized."})
	}}
	if _, err := r.compactContext(context.Background(), "", r.CurrentSelection(), &warning, true); err != nil {
		t.Fatal(err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, message := range messages {
		if reflect.DeepEqual(message, warning) {
			found++
		}
	}
	if found != 1 {
		t.Fatal("forced handoff lost or duplicated the pending recovery warning", found)
	}
}

func TestForceCompactDoesNotBypassSummaryOrHandoffSafety(t *testing.T) {
	for _, mode := range []string{"handoff", "oversized", "empty", "tool", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			r := forceCompactionFixture(t)
			source := r.Current()
			original, err := r.Store.Messages(source)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			r.Provider = &childProvider{stream: func(got context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				switch mode {
				case "handoff":
					return emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("Still too large for final coding handoff. ", 10000)})
				case "oversized":
					return emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("x", (1<<20)+1)})
				case "empty":
					return emit(llm.StreamEvent{Kind: "text", Text: "  \n "})
				case "tool":
					return emit(llm.StreamEvent{Kind: "call_start", Call: &llm.ToolCall{ID: "forbidden", Name: "shell"}})
				default:
					if err := emit(llm.StreamEvent{Kind: "text", Text: "Valid but canceled summary."}); err != nil {
						return err
					}
					cancel()
					return got.Err()
				}
			}}
			_, err = r.compactContext(ctx, "", r.CurrentSelection(), nil, true)
			fatal := mode == "empty" || mode == "tool"
			if err == nil || calls != 1 || r.Current() != source || (r.checkContext(false) != nil) != fatal || recoverableCompaction(err) == fatal {
				t.Fatal("force bypassed existing failure policy", calls, err, r.checkContext(false))
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("force lost owning cancellation", err)
			}
			retained, readErr := r.Store.Messages(source)
			if readErr != nil || !reflect.DeepEqual(retained, original) {
				t.Fatal("failed force replaced original model context", readErr)
			}
		})
	}
}

func TestForceCompactCommandKeepsNormalGuards(t *testing.T) {
	for _, command := range []string{"/compact", "/force-compact"} {
		for _, guard := range []string{"empty", "read-only", "fatal"} {
			t.Run(command+"/"+guard, func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				if guard != "empty" {
					seedCompactionHistory(t, r, "Earlier completed evidence.")
				}
				if guard == "read-only" {
					if _, err := historytest.DB(t, r.Store).Exec("UPDATE sessions SET read_only=1 WHERE id=?", r.Current()); err != nil {
						t.Fatal(err)
					}
				}
				if guard == "fatal" {
					r.fatalCompaction = "previous invariant failure"
				}
				calls, source := 0, r.Current()
				r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error { calls++; return nil }}
				want := guard
				if guard == "fatal" {
					want = "unusable"
				}
				if _, err := r.Command(command); err == nil || !strings.Contains(err.Error(), want) || calls != 0 || r.Current() != source {
					t.Fatal("manual command bypassed existing context guard", calls, err)
				}
				if guard == "read-only" && r.checkContext(false) != nil {
					t.Fatal("refusing read-only compaction invalidated the archived context")
				}
			})
		}
	}
}

type forceReplayProvider struct {
	childProvider
	estimates atomic.Int32
}

func (p *forceReplayProvider) EstimateReplay(llm.Message) int {
	p.estimates.Add(1)
	return 1 << 30
}

func TestForceSummarySkipsInputEstimation(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Research")
			selection := r.CurrentSelection()
			message := llm.Message{Type: llm.TypeAssistant, Content: "Original evidence.", State: &llm.ReplayState{Provider: selection.Provider, Model: selection.Model.RequestID(), Version: 1, Items: []json.RawMessage{json.RawMessage(`{"native":"reasoning"}`)}}}
			calls := 0
			p := &forceReplayProvider{childProvider: childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				if !reflect.DeepEqual(req.Messages[0], message) {
					t.Fatal("summary changed original replay input")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Summary."})
			}}}
			r.Provider = p
			snapshot := contextbuild.InputSnapshot{Selection: selection, System: systemTemplate, Messages: []llm.Message{message}, Lineage: r.Current()}
			text, err := r.summarize(context.Background(), "main", "", snapshot, &r.estimator, "", force)
			if force {
				if err != nil || text != "Summary." || calls != 1 || p.estimates.Load() != 0 {
					t.Fatal("forced summary performed input estimation", text, err, calls, p.estimates.Load())
				}
			} else if err == nil || !strings.Contains(err.Error(), "estimated input") || calls != 0 || p.estimates.Load() != 1 {
				t.Fatal("normal summary bypassed replay estimate capacity refusal", err, calls, p.estimates.Load())
			}
			var requests int
			if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM model_requests WHERE purpose='compaction'").Scan(&requests); err != nil || requests != calls {
				t.Fatal("summary request persisted before preflight", requests, calls, err)
			}
		})
	}
}

func TestSummaryRejectsInvalidProjectionBeforeRequest(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Research")
			calls := 0
			r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error { calls++; return nil }}
			for _, message := range []llm.Message{
				{Type: "unknown"},
				{Type: llm.TypeUserInput, Calls: []llm.ToolCall{{ID: "invalid", Name: "read"}}},
				{Type: llm.TypeAssistant, RetainedAtMS: 1000},
				{Type: llm.TypeUserInput, RetainedAtMS: 1000},
			} {
				snapshot := contextbuild.InputSnapshot{Selection: r.CurrentSelection(), System: systemTemplate, Messages: []llm.Message{message}, Lineage: r.Current()}
				if _, err := r.summarize(context.Background(), "main", "", snapshot, &r.estimator, "", force); err == nil || calls != 0 {
					t.Fatal("invalid semantic/projection input reached provider", message, calls, err)
				}
			}
			var requests int
			if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM model_requests WHERE purpose='compaction'").Scan(&requests); err != nil || requests != 0 {
				t.Fatal("invalid input created a summary request", requests, err)
			}
		})
	}
}

func TestForceCompactHTTPContextRejectionPreservesContextLiveWorkAndQueues(t *testing.T) {
	r := forceCompactionFixture(t)
	source := r.Current()
	original, err := r.Store.Messages(source)
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.Jobs.Start("main", "sleep 30", r.Workspace.Root, 0, true, false, false, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	timer, err := r.timers.schedule(wakeupSchedule{Name: "force-context-rejection", Message: "Still live"}, "main", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.queueNotification(`{"type":"job_exit","job_id":"pending_force"}`); err != nil {
		t.Fatal(err)
	}
	r.steers = []contextbuild.Input{{Text: "Preserve the pending instruction.", Source: "steer"}}
	notices := append([]llm.Message(nil), r.notifications...)
	steers := append([]contextbuild.Input(nil), r.steers...)
	var attempts atomic.Int32
	installRetryHTTPAdapter(t, r, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		attempt := attempts.Add(1)
		w.Header().Set("Retry-After", "0")
		w.Header().Set("x-request-id", fmt.Sprintf("force-context-%d", attempt))
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","message":"Reduce input or compact the conversation."}}`)
	}))
	_, err = r.Command("/force-compact prioritize evidence")
	var upstream *llm.TransientError
	if !errors.As(err, &upstream) || !recoverableCompaction(err) || attempts.Load() != llm.DefaultMaxAttempts || r.Current() != source || r.checkContext(false) != nil {
		t.Fatal("forced upstream 400 context rejection did not remain recoverable", attempts.Load(), err)
	}
	retained, readErr := r.Store.Messages(source)
	if readErr != nil || !reflect.DeepEqual(retained, original) || !reflect.DeepEqual(r.notifications, notices) || !reflect.DeepEqual(r.steers, steers) {
		t.Fatal("upstream rejection changed prior context or consumed queues", readErr)
	}
	if live := r.Jobs.Live(); len(live) != 1 || live[0].ID != job {
		t.Fatal("upstream rejection canceled unrelated live job", live)
	}
	if live := r.timers.list(); len(live) != 1 || live[0].ID != timer.ID || live[0].Status != "scheduled" {
		t.Fatal("upstream rejection canceled unrelated timer", live)
	}
	var status, attemptStatus, diagnostic string
	if err := historytest.DB(t, r.Store).QueryRow("SELECT status,json_extract(attempts_json,'$[0].status'),json_extract(attempts_json,'$[0].error') FROM model_requests WHERE session_id=? AND purpose='compaction'", source).Scan(&status, &attemptStatus, &diagnostic); err != nil || status != "failed" || attemptStatus != "failed" || diagnostic != upstream.Error() || !strings.Contains(diagnostic, "context_length_exceeded") || !strings.Contains(diagnostic, "force-context-3") {
		t.Fatal("forced rejection lost durable request-failed diagnostics", status, attemptStatus, diagnostic, err)
	}
	var forced int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM entries WHERE session_id=? AND json_extract(content_json,'$.type')='compaction_started' AND json_extract(content_json,'$.force')=1", source).Scan(&forced); err != nil || forced != 1 {
		t.Fatal("forced attempt lost durable boundary metadata", forced, err)
	}
}
