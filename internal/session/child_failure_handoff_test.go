package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func assertChildFailureHandoff(t *testing.T, r *Runtime, result map[string]any, status, answer string) {
	t.Helper()
	if result["status"] != status || result["answer"] != answer || result["stdout"] != nil {
		t.Fatalf("wrong terminal handoff: %#v", result)
	}
	warning, _ := result["warning"].(string)
	if !strings.Contains(warning, "Partial work may have side effects") || !strings.Contains(warning, "not a completed result") {
		t.Fatal("missing explicit partial-work warning", result)
	}
	path, _ := result["transcript_path"].(string)
	exactPath, _ := result["transcript_jsonl_path"].(string)
	if path == "" || exactPath != path+".jsonl" || !strings.Contains(warning, path) || !strings.Contains(warning, exactPath) {
		t.Fatal("missing durable transcript references", result)
	}
	session, err := r.Store.Session(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(r.Store.Root, "lineages", session.LineageID)
	if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		t.Fatal("transcript is not a lineage artifact", path, err)
	}
	for _, name := range []string{path, exactPath} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("transcript missing or not private", name, info, err)
		}
	}
	if len(r.ChildViews("main")) != 0 {
		t.Fatal("failed child remained reusable")
	}
	var savedStatus string
	if err := r.Store.DB.QueryRow("SELECT status FROM turns WHERE id=?", result["child_turn_id"]).Scan(&savedStatus); err != nil {
		t.Fatal(err)
	}
	wantSaved := status
	if status == "cancelled" {
		wantSaved = "interrupted"
	}
	if savedStatus != wantSaved {
		t.Fatal("failure was persisted as success", savedStatus)
	}
}

func TestChildFailureHandoffForegroundLastAssistantNotCapturedTranscript(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	requests := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if requests == 1 {
			if err := emit(llm.StreamEvent{Kind: "text", Text: "Earlier tool commentary must not be returned."}); err != nil {
				return err
			}
			call := llm.ToolCall{ID: "effect", Name: "write", Arguments: []byte(`{"path":"partial.txt","content":"a side effect\n"}`)}
			return emit(llm.StreamEvent{Kind: "call", Call: &call})
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Last partial assistant evidence"}); err != nil {
			return err
		}
		return errors.New("upstream exhausted")
	}}
	args := `{"persistent":true,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	result := childInvocation(t, r, ids[0], args)
	assertChildFailureHandoff(t, r, result, "failed", "Last partial assistant evidence")
	if result["ok"] != true || result["error"] != "upstream exhausted" || r.HasNotifications() {
		t.Fatal("foreground completion incorrectly delivered", result)
	}
	if raw, err := os.ReadFile(filepath.Join(r.Workspace.Root, "partial.txt")); err != nil || string(raw) != "a side effect\n" {
		t.Fatal("partial side effect missing", string(raw), err)
	}
	raw, err := os.ReadFile(result["transcript_jsonl_path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Earlier tool commentary", "Last partial assistant evidence", "partial.txt", `"tool_record"`, `"instructions"`} {
		if !strings.Contains(string(raw), needle) {
			t.Fatal("complete transcript lost content", needle)
		}
	}
	entry, err := r.Store.Entry(int64(result["result_entry_id"].(float64)))
	if err != nil || !strings.Contains(string(entry.Content), "Last partial assistant evidence") {
		t.Fatal("wrong last assistant entry", entry, err)
	}
}

func TestChildFailureHandoffNoTextAndEmptySuccessfulStream(t *testing.T) {
	for _, streamError := range []error{errors.New("local provider failure"), nil} {
		t.Run(map[bool]string{true: "error", false: "empty_completion"}[streamError != nil], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error { return streamError }}
			args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
			result := childInvocation(t, r, ids[0], args)
			assertChildFailureHandoff(t, r, result, "failed", "No assistant text was produced for this assignment.")
			if result["error"] == nil || result["result_entry_id"] != float64(0) {
				t.Fatal("empty completion fabricated a result", result)
			}
		})
	}
}

func TestChildFailureHandoffBoundedAnswerKeepsWarningAndPaths(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	text := "Last evidence: " + strings.Repeat("界", history.MaxChildAnswerBytes)
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: text}); err != nil {
			return err
		}
		return errors.New("upstream exhausted")
	}}
	args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	result := childInvocation(t, r, ids[0], args)
	answer, ok := result["answer"].(string)
	if !ok || len(answer) > history.MaxChildAnswerBytes || !utf8.ValidString(answer) || !strings.HasPrefix(text, answer) || result["answer_truncated"] != true {
		t.Fatal("invalid bounded partial answer", result)
	}
	assertChildFailureHandoff(t, r, result, "failed", answer)
	raw, err := os.ReadFile(result["transcript_jsonl_path"].(string))
	if err != nil || !strings.Contains(string(raw), text) {
		t.Fatal("complete partial text missing from export", err)
	}
}

func TestChildFailureHandoffBackgroundFailureAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{true: "cancelled", false: "failed"}[cancelled], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			started, release := make(chan struct{}), make(chan struct{})
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if err := emit(llm.StreamEvent{Kind: "text", Text: "Background partial evidence"}); err != nil {
					return err
				}
				close(started)
				select {
				case <-release:
					return errors.New("upstream exhausted")
				case <-ctx.Done():
					return ctx.Err()
				}
			}}
			args := `{"persistent":true,"prompt":"investigate","label":"audit","background":true}`
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
			launch := childInvocation(t, r, ids[0], args)
			if launch["answer"] != nil || launch["stdout"] != nil || launch["warning"] != nil || launch["transcript_path"] != nil {
				t.Fatal("background launch duplicated handoff", launch)
			}
			receive(t, started)
			status := "failed"
			if cancelled {
				status = "cancelled"
				if _, err := r.StopChild(context.Background(), "main", launch["child_id"].(string)); err != nil {
					t.Fatal(err)
				}
			} else {
				close(release)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := r.Jobs.Wait(ctx, "main", launch["job_id"].(string), nil); err != nil {
					t.Fatal(err)
				}
			}
			r.orderMu.Lock()
			notifications := append([]llm.Message(nil), r.notifications...)
			r.orderMu.Unlock()
			if len(notifications) != 1 {
				t.Fatal("completion did not notify exactly once", notifications)
			}
			var finish map[string]any
			if err := json.Unmarshal([]byte(notifications[0].Content), &finish); err != nil {
				t.Fatal(err)
			}
			assertChildFailureHandoff(t, r, finish, status, "Background partial evidence")
			if finish["type"] != "child_turn_finished" || finish["error"] == nil || finish["result_entry_id"] == nil {
				t.Fatal("incomplete background failure", finish)
			}
		})
	}
}

func TestChildFailureHandoffExportFailureIsExplicit(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		// System instructions are already durable; block only transcript archive storage.
		saved, err := r.Store.Session(r.Current())
		if err != nil {
			return err
		}
		path := filepath.Join(r.Store.Root, "lineages", saved.LineageID, "compactions")
		if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
			return err
		}
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Useful partial evidence"}); err != nil {
			return err
		}
		return errors.New("upstream exhausted")
	}}
	args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	result := childInvocation(t, r, ids[0], args)
	warning, _ := result["warning"].(string)
	if result["status"] != "failed" || result["answer"] != "Useful partial evidence" || result["transcript_export_error"] == nil || result["transcript_path"] != nil || result["transcript_jsonl_path"] != nil || !strings.Contains(warning, "transcript export failed") || !strings.Contains(warning, "may have side effects") {
		t.Fatal("export failure was swallowed or invented a path", result)
	}
}

func TestChildFailureHandoffLocalStorageFailureExportsUncommittedFullText(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_child_request_finish BEFORE UPDATE OF status ON model_requests
WHEN NEW.actor_id != 'main' AND NEW.status != 'running'
BEGIN SELECT RAISE(ABORT,'injected local request finish failure'); END`); err != nil {
		t.Fatal(err)
	}
	text := "Uncommitted partial evidence: " + strings.Repeat("界", history.MaxChildAnswerBytes)
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: text})
	}}
	args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	result := childInvocation(t, r, ids[0], args)
	answer, _ := childAnswer(text)
	assertChildFailureHandoff(t, r, result, "failed", answer)
	if result["result_entry_id"] != float64(0) || result["answer_truncated"] != true || !strings.Contains(result["error"].(string), "injected local request finish failure") {
		t.Fatal("local storage failure invented an assistant entry", result)
	}
	exact, err := os.ReadFile(result["transcript_jsonl_path"].(string))
	if err != nil || !strings.Contains(string(exact), text) || !strings.Contains(string(exact), `"uncommitted":true`) {
		t.Fatal("uncommitted full output missing or mislabeled", err)
	}
	markdown, err := os.ReadFile(result["transcript_path"].(string))
	if err != nil || !strings.Contains(string(markdown), text) || !strings.Contains(string(markdown), "Uncommitted assistant output") {
		t.Fatal("uncommitted Markdown missing or mislabeled", err)
	}
}

func TestChildFailureHandoffPersistentFollowupKeepsLastTextWithExplicitAttribution(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	requests := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if requests == 1 {
			return emit(llm.StreamEvent{Kind: "text", Text: "Previous assignment's actual answer"})
		}
		return errors.New("upstream failed before any new text")
	}}
	args := `{"persistent":true,"prompt":"first assignment","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	first := childInvocation(t, r, ids[0], args)
	if first["status"] != "completed" {
		t.Fatal(first)
	}
	followup, _ := json.Marshal(map[string]any{"persistent": true, "child_id": first["child_id"], "prompt": "second assignment"})
	result := childInvocation(t, r, ids[0], string(followup))
	assertChildFailureHandoff(t, r, result, "failed", "Previous assignment's actual answer")
	if result["result_entry_id"] != first["result_entry_id"] || !strings.Contains(result["warning"].(string), "from the previous assignment") {
		t.Fatal("previous text was lost or presented as new work", result)
	}
	exact, err := os.ReadFile(result["transcript_jsonl_path"].(string))
	if err != nil || !strings.Contains(string(exact), "first assignment") || !strings.Contains(string(exact), "second assignment") || !strings.Contains(string(exact), "Previous assignment's actual answer") {
		t.Fatal("persistent conversation missing from export", err)
	}
}

func TestChildFailureHandoffForegroundCancellationReturnsPartialText(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if err := emit(llm.StreamEvent{Kind: "text", Text: "Foreground partial evidence"}); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan tool.Record, 1)
	go func() {
		finished <- r.Tools.Invoke(ctx, tool.Execution{SessionID: r.Current(), Actor: "main", CallID: ids[0]}, "subagent", []byte(args))
	}()
	receive(t, started)
	cancel()
	var record tool.Record
	select {
	case record = <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("foreground cancellation did not join the child")
	}
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	assertChildFailureHandoff(t, r, result, "cancelled", "Foreground partial evidence")
	if result["error"] != context.Canceled.Error() || r.HasNotifications() {
		t.Fatal("foreground cancellation was not explicit or notified twice", result)
	}
}

func TestChildFailureHandoffLocalLaunchFailureForegroundAndBackground(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(map[bool]string{true: "background", false: "foreground"}[background], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			r.Jobs.Close()
			args, _ := json.Marshal(map[string]any{"persistent": false, "prompt": "investigate", "label": "audit", "background": background})
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: args}})
			result := childInvocation(t, r, ids[0], string(args))
			if result["status"] != "failed" {
				t.Fatal("local launch failure fabricated a running child", result)
			}
			if background {
				if result["answer"] != nil || result["stdout"] != nil {
					t.Fatal("launch duplicated background handoff", result)
				}
				r.orderMu.Lock()
				notifications := append([]llm.Message(nil), r.notifications...)
				r.orderMu.Unlock()
				if len(notifications) != 1 {
					t.Fatal("local launch failure did not notify once", notifications)
				}
				if err := json.Unmarshal([]byte(notifications[0].Content), &result); err != nil {
					t.Fatal(err)
				}
			}
			assertChildFailureHandoff(t, r, result, "failed", "No assistant text was produced for this assignment.")
			if result["error"] == nil || result["job_id"] != "" {
				t.Fatal("launch failure omitted its cause or fabricated a job", result)
			}
		})
	}
}

func TestChildFailureHandoffCompletionStorageFailureStillWarnsAndExports(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_child_completion BEFORE INSERT ON entries
WHEN json_extract(NEW.content_json,'$.type')='child_turn_finished'
BEGIN SELECT RAISE(ABORT,'injected child completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: "Last assistant evidence before local completion failure"})
	}}
	args := `{"persistent":false,"prompt":"investigate","label":"audit"}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	result := childInvocation(t, r, ids[0], args)
	warning, _ := result["warning"].(string)
	path, _ := result["transcript_path"].(string)
	if result["status"] != "failed" || result["answer"] != "Last assistant evidence before local completion failure" || result["stdout"] != nil || result["finish_event_seq"] != nil || path == "" || !strings.Contains(warning, "may have side effects") || !strings.Contains(warning, "runtime cannot continue") {
		t.Fatal("storage failure lost the handoff or fabricated committed completion", result)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("durable investigation transcript missing", err)
	}
	r.orderMu.Lock()
	orderError := r.orderError
	r.orderMu.Unlock()
	if orderError == nil || !strings.Contains(orderError.Error(), "injected child completion failure") || !strings.Contains(orderError.Error(), path) {
		t.Fatal("fatal storage error omitted the investigation path", orderError)
	}
}
