package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func TestSubagentReadGrantDeliveryAndLifetime(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	id, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "fixture"}, func(_ context.Context, out, _ io.Writer) (jobs.TaskResult, error) {
		_, _ = io.WriteString(out, "fixture")
		return jobs.TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	const prompt = "  {\"prompt\":\"authored task\",\"job_read_grants\":[\"not a capability\"]}\n"
	streams := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		streams++
		foundTask, grants := false, 0
		for _, m := range req.Messages {
			if m.Type == llm.TypeUserInput && m.InputSource == "task" && m.Content == prompt {
				foundTask = true
			}
			if m.Type == llm.TypeRuntimeNotification && strings.Contains(m.Content, "job_read_grants") {
				assertAssignmentGrantNotice(t, m, []string{id})
				grants++
			}
		}
		if !foundTask || grants != 1 {
			t.Errorf("assignment text changed or grant notice duplicated: task=%v notices=%d", foundTask, grants)
		}
		if _, err := r.Jobs.Read(ctx, req.ConversationID, id, jobs.ReadOptions{Limit: 100}); err != nil {
			t.Error(err)
		}
		if streams == 1 {
			if err := emit(llm.StreamEvent{Kind: "text", Text: "partial"}); err != nil {
				return err
			}
			return partialRetryFailure(req)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "done"})
	}}
	maxIDs := make([]string, maxJobReadGrants)
	for i := range maxIDs {
		maxIDs[i] = id
	}
	args, _ := json.Marshal(map[string]any{"persistent": true, "prompt": prompt, "label": "reader", "job_read_grants": maxIDs})
	_, calls := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: args}})
	invalid, _ := json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": "reader", "job_read_grants": []string{id, "missing"}})
	if result := childInvocation(t, r, calls[0], string(invalid)); result["ok"] == true || len(r.ChildViews("main")) != 0 {
		t.Fatal("invalid mixed grant launched a child", result)
	}
	oversized := append(append([]string(nil), maxIDs...), id)
	invalid, _ = json.Marshal(map[string]any{"persistent": true, "prompt": "task", "label": "reader", "job_read_grants": oversized})
	if result := childInvocation(t, r, calls[0], string(invalid)); result["ok"] == true || !strings.Contains(fmt.Sprint(result), "at most 128 IDs") || len(r.ChildViews("main")) != 0 {
		t.Fatal("oversized grant launched a child", result)
	}
	child := childInvocation(t, r, calls[0], string(args))
	actor := child["child_id"].(string)
	if _, err := r.Jobs.Read(context.Background(), actor, id, jobs.ReadOptions{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	// A retained context keeps the capability across an assignment boundary.
	args, _ = json.Marshal(map[string]any{"persistent": true, "prompt": "task", "child_id": actor})
	childInvocation(t, r, calls[0], string(args))
	if streams != 3 {
		t.Fatal("expected a partial retry and retained follow-up", streams)
	}
	assertStoredAssignmentGrants(t, r, actor, []string{id}, 1)
	mainMessages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range mainMessages {
		if message.Type == llm.TypeRuntimeNotification && strings.Contains(message.Content, "job_read_grants") {
			t.Fatal("assignment grant notice leaked into main", message)
		}
	}
	if _, err := r.StopChild(context.Background(), "main", actor); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Jobs.Read(context.Background(), actor, id, jobs.ReadOptions{Limit: 100}); err == nil {
		t.Fatal("grant survived child closure")
	}
}

func TestInitialAssignmentReadGrantsRequireSuccessfulAdmission(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	job, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "fixture"}, func(context.Context, io.Writer, io.Writer) (jobs.TaskResult, error) {
		return jobs.TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Jobs.Wait(context.Background(), "main", job, nil); err != nil {
		t.Fatal(err)
	}
	const actor, prompt = "main/child_admission_grants", "  exact task\n"
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	assignment := &childAssignment{initialReadGrants: []string{job}}
	task := childTask{actor: actor, turn: turn, prompt: prompt, assignment: assignment, selection: r.selection, tools: r.Tools, estimator: &contextbuild.Estimator{}}
	r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error {
		t.Error("provider called after failed admission")
		return nil
	}}
	db := historytest.DB(t, r.Store)
	if _, err := db.Exec(`CREATE TRIGGER reject_initial_grant_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'injected admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.runChild(context.Background(), task, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "injected admission failure") {
		t.Fatal("expected admission failure", err)
	}
	if _, err := r.Jobs.Read(context.Background(), actor, job, jobs.ReadOptions{Limit: 100}); err == nil {
		t.Fatal("failed admission granted read access")
	}
	if !reflect.DeepEqual(assignment.initialReadGrants, []string{job}) {
		t.Fatal("failed admission consumed initial grants", assignment.initialReadGrants)
	}
	assertStoredAssignmentGrants(t, r, actor, []string{job}, 1)
	var stored string
	if err := db.QueryRow(`SELECT content_json FROM entries WHERE actor_id=? AND json_extract(content_json,'$.input_source')='task'`, actor).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var question llm.Message
	if err := json.Unmarshal([]byte(stored), &question); err != nil || question.Content != prompt || question.Type != llm.TypeUserInput {
		t.Fatal("canonical task was changed", question, err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_initial_grant_admission`); err != nil {
		t.Fatal(err)
	}
	// Retrying admission reuses the already-persisted task and notice. It must
	// neither acknowledge the notice as a source nor persist a second copy.
	var noticeJSON string
	if err := db.QueryRow(`SELECT content_json FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='runtime_notification'`, actor).Scan(&noticeJSON); err != nil {
		t.Fatal(err)
	}
	var notice llm.Message
	if err := json.Unmarshal([]byte(noticeJSON), &notice); err != nil {
		t.Fatal(err)
	}
	admitted, _, err := r.admitChild(context.Background(), task, []llm.Message{question, notice}, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Jobs.Read(context.Background(), actor, job, jobs.ReadOptions{Limit: 100}); err != nil {
		t.Fatal("successful admission did not apply grant", err)
	}
	if len(assignment.initialReadGrants) != 0 || len(admitted.NoticeEntries) != 0 || len(admitted.Notifications) != 0 {
		t.Fatal("assignment grant notice entered source-event delivery", assignment.initialReadGrants, admitted)
	}
	assertStoredAssignmentGrants(t, r, actor, []string{job}, 1)
	if err := r.Store.FinishRequest(admitted.RequestID, history.RequestCompletion{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
}

func assertAssignmentGrantNotice(t *testing.T, message llm.Message, ids []string) {
	t.Helper()
	if message.Type != llm.TypeRuntimeNotification || message.EventSeq != 0 || message.InputSource != "" {
		t.Errorf("assignment grant notice has source-event or task provenance: %#v", message)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message.Content), &body); err != nil {
		t.Fatal(err)
	}
	var kind string
	var granted []string
	if err := json.Unmarshal(body["type"], &kind); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body["job_ids"], &granted); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || kind != "job_read_grants" || !reflect.DeepEqual(granted, ids) {
		t.Errorf("noncanonical grant notice: %s", message.Content)
	}
	projected, err := message.ModelContent()
	if err != nil || strings.Count(projected, "<runtime_notification>") != 1 || !strings.Contains(projected, ids[0]) {
		t.Errorf("grant notice did not use shared runtime projection: %q (%v)", projected, err)
	}
}

func assertStoredAssignmentGrants(t *testing.T, r *Runtime, actor string, ids []string, want int) {
	t.Helper()
	db := historytest.DB(t, r.Store)
	rows, err := db.Query(`SELECT id,model_visible,content_json FROM entries WHERE actor_id=? AND kind='message' AND json_extract(content_json,'$.type')='runtime_notification' ORDER BY id`, actor)
	if err != nil {
		t.Fatal(err)
	}
	var entries []int64
	for rows.Next() {
		var entry int64
		var visible bool
		var content string
		if err := rows.Scan(&entry, &visible, &content); err != nil {
			t.Fatal(err)
		}
		var message llm.Message
		if err := json.Unmarshal([]byte(content), &message); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(message.Content, "job_read_grants") {
			continue
		}
		assertAssignmentGrantNotice(t, message, ids)
		if visible {
			t.Error("child grant notice visible in main history")
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("stored grant notices = %d, want %d", len(entries), want)
	}
	for _, entry := range entries {
		var acknowledged int
		if err := db.QueryRow(`SELECT count(*) FROM model_requests,json_each(delivered_events_json) WHERE actor_id=? AND json_each.value=?`, actor, entry).Scan(&acknowledged); err != nil || acknowledged != 0 {
			t.Fatal("grant metadata was acknowledged as a source event", acknowledged, err)
		}
	}
}
