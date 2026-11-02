package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/tool"
)

func sendChildSteer(t *testing.T, r *Runtime, actor, call string, args map[string]any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(args)
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: actor, CallID: call}, "child_steer", data)
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestChildSteerRegistrationAndTargetValidation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	steer, ok := r.Tools.Get("child_steer")
	if !ok || steer.Definition().Description != prompts.ToolDescription("child_steer") {
		t.Fatal("child_steer missing its registered tool description")
	}
	if _, ok := r.Tools.Get("child_message"); ok {
		t.Fatal("obsolete child_message tool alias remains registered")
	}
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: "main"}, "child_message", []byte(`{"job_id":"job","message":"guidance"}`))
	var rejected struct {
		OK    bool       `json:"ok"`
		Error tool.Error `json:"error"`
	}
	if err := json.Unmarshal(record.Result, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.OK || rejected.Error.Code != "unknown_tool" {
		t.Fatalf("obsolete tool name was not rejected: %s", record.Result)
	}
	for _, args := range []string{
		`{"message":"guidance"}`,
		`{"child_id":"child","message":"guidance"}`,
		`{"child_turn_id":"turn","message":"guidance"}`,
	} {
		if _, err := steer.DecodeCall(1, []byte(args)); err == nil || err.Error() != tool.Fail("invalid_arguments", prompts.ChildMessageTarget).Error() {
			t.Errorf("incomplete assignment selector %s: %v", args, err)
		}
	}
	for _, args := range []string{
		`{"job_id":"job","message":"guidance"}`,
		`{"child_id":"child","child_turn_id":"turn","message":"guidance"}`,
		`{"job_id":"job","child_id":"child","message":"guidance"}`,
		`{"job_id":"job","child_id":"child","child_turn_id":"turn","message":"guidance"}`,
	} {
		if _, err := steer.DecodeCall(1, []byte(args)); err != nil {
			t.Errorf("valid assignment selector %s: %v", args, err)
		}
	}
}

func TestChildSteerMessagesFIFOContinueSameAssignment(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		n := requests.Add(1)
		if n == 1 {
			close(started)
			<-release
			// The provider may reuse this exact input for internal retries.
			// Parent sends must not mutate an already admitted request.
			for _, message := range req.Messages {
				if strings.Contains(message.Content, `"type":"child_message"`) {
					t.Error("queued guidance mutated a frozen request")
				}
			}
		} else {
			var got []string
			for _, m := range req.Messages {
				if m.EventSeq != 0 && strings.Contains(m.Content, `"type":"child_message"`) {
					got = append(got, m.Content)
					projected, err := m.ModelContent()
					if err != nil {
						t.Fatal(err)
					}
					var guidance struct {
						Message string `json:"message"`
					}
					if err := json.Unmarshal([]byte(m.Content), &guidance); err != nil {
						t.Fatal(err)
					}
					if m.Role() != "user" || projected != "<steer>\n"+guidance.Message+"\n</steer>" {
						t.Fatal("child did not receive concise steering", projected)
					}
				}
			}
			if len(got) != 2 || !strings.Contains(got[0], `"message":"first"`) || !strings.Contains(got[1], `"message":"second"`) {
				t.Errorf("queued messages not FIFO: %v", got)
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "answer"})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"task","label":"research","background":true}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":true,"prompt":"task","label":"research","background":true}`)
	receive(t, started)
	for _, text := range []string{"first", "second"} {
		result := sendChildSteer(t, r, "main", ids[0], map[string]any{"child_id": first["child_id"], "child_turn_id": first["child_turn_id"], "message": text})
		if result["ok"] != true || result["state"] != "queued" || result["delivered"] != false {
			t.Fatal(result)
		}
	}
	if result := sendChildSteer(t, r, "main", ids[0], map[string]any{"child_id": first["child_id"], "child_turn_id": "stale", "message": "no"}); result["ok"] == true {
		t.Fatal(result)
	}
	if result := sendChildSteer(t, r, first["child_id"].(string), ids[0], map[string]any{"job_id": first["job_id"], "message": "no"}); result["ok"] == true {
		t.Fatal(result)
	}
	close(release)
	if _, err := r.Jobs.Wait(context.Background(), "main", first["job_id"].(string), nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal(requests.Load())
	}
	var delivered, childRows int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message' AND delivered_request_id IS NOT NULL`).Scan(&delivered); err != nil || delivered != 2 {
		t.Fatal(delivered, err)
	}
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE actor_id=? AND kind='message' AND json_extract(content_json,'$.event_seq') IS NOT NULL`, first["child_id"]).Scan(&childRows); err != nil || childRows != 2 {
		t.Fatal(childRows, err)
	}
	if result := sendChildSteer(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "too late"}); result["ok"] == true {
		t.Fatal(result)
	}
}

func TestChildSteerCancellationReportsUndelivered(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started := make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"task","label":"research","background":true}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"task","label":"research","background":true}`)
	receive(t, started)
	result := sendChildSteer(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "original evidence"})
	if result["state"] != "queued" {
		t.Fatal(result)
	}
	if _, err := r.StopChild(context.Background(), "main", first["child_id"].(string)); err != nil {
		t.Fatal(err)
	}
	var undelivered int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.body.type')='child_message_undelivered'`).Scan(&undelivered); err != nil || undelivered != 1 {
		t.Fatal(undelivered, err)
	}
	var acknowledged int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message' AND delivered_request_id IS NOT NULL`).Scan(&acknowledged); err != nil || acknowledged != 0 {
		t.Fatal(acknowledged, err)
	}
}

func TestChildSteerWaitsForForegroundBatch(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	started, release := make(chan struct{}), make(chan struct{})
	tool.Register(r.Tools, "child_block", "test block", nil, nil, func(a struct{}) error { return nil }, func(ctx context.Context, x tool.Execution, a struct{}) (any, error) {
		close(started)
		<-release
		return map[string]bool{"settled": true}, nil
	})
	var requests atomic.Int32
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		if requests.Add(1) == 1 {
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "block", Name: "child_block", Arguments: []byte(`{}`)}})
		}
		toolIndex, noticeIndex := -1, -1
		for i, message := range req.Messages {
			if message.Type == llm.TypeToolResult && message.CallID == "block" {
				toolIndex = i
			}
			if strings.Contains(message.Content, `"type":"child_message"`) {
				noticeIndex = i
			}
		}
		if toolIndex < 0 || noticeIndex <= toolIndex {
			t.Errorf("unsafe message order: tool=%d notice=%d", toolIndex, noticeIndex)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "done"})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"task","label":"research","background":true}`)}})
	first := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"task","label":"research","background":true}`)
	receive(t, started)
	result := sendChildSteer(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "after tools"})
	if result["state"] != "queued" {
		t.Fatal(result)
	}
	if requests.Load() != 1 {
		t.Fatal("request admitted before tools settled")
	}
	close(release)
	if _, err := r.Jobs.Wait(context.Background(), "main", first["job_id"].(string), nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal(requests.Load())
	}
}

func TestChildSteerQueuedDuringCompaction(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		name := "fits"
		if oversized {
			name = "oversized"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			tool.Register(r.Tools, "child_noop", "test noop", nil, nil, func(a struct{}) error { return nil }, func(ctx context.Context, x tool.Execution, a struct{}) (any, error) {
				return map[string]bool{"settled": true}, nil
			})
			compactionBudget(t, r)
			// Allow the first cycle to be consumed before compaction; fresh
			// results cannot be summarized just to manufacture older work.
			r.selection.Model.Budget.ContextLimit += 6000
			started, release := make(chan struct{}), make(chan struct{})
			var coding, summaries atomic.Int32
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries.Add(1)
					close(started)
					<-release
					return emit(llm.StreamEvent{Kind: "text", Text: "Concise child handoff."})
				}
				if n := coding.Add(1); n <= 2 {
					if err := emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("Old child research notes. ", 700)}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: fmt.Sprintf("noop-%d", n), Name: "child_noop", Arguments: []byte(`{}`)}})
				}
				count := 0
				for _, message := range req.Messages {
					if strings.Contains(message.Content, `"type":"child_message"`) {
						count++
						if !message.Type.IsRuntime() || message.InputSource != "" {
							t.Error("parent guidance became a retained human prompt")
						}
					}
				}
				want := 1
				if oversized {
					want = 0
				}
				if count != want {
					t.Errorf("queued guidance count = %d", count)
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "done"})
			}}
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"task","label":"research","background":true}`)}})
			first := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"task","label":"research","background":true}`)
			receive(t, started)
			text := "guidance arriving while summarizing"
			if oversized {
				text = strings.Repeat("large parent instruction ", 5000)
			}
			result := sendChildSteer(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": text})
			if oversized {
				if result["ok"] == true || !strings.Contains(fmt.Sprint(result), "message_queue_capacity") {
					t.Fatal(result)
				}
			} else if result["state"] != "queued" {
				t.Fatal(result)
			}
			close(release)
			view, err := r.Jobs.Wait(context.Background(), "main", first["job_id"].(string), nil)
			if err != nil {
				t.Fatal(err)
			}
			if summaries.Load() != 1 {
				t.Fatal("repeated compaction", summaries.Load())
			}
			var delivered int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message' AND delivered_request_id IS NOT NULL`).Scan(&delivered); err != nil {
				t.Fatal(err)
			}
			if oversized {
				if coding.Load() != 3 || delivered != 0 || view.Status != "completed" {
					t.Fatal(coding.Load(), delivered, view)
				}
				var undelivered int
				if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.body.type')='child_message_undelivered'`).Scan(&undelivered); err != nil || undelivered != 0 {
					t.Fatal(undelivered, err)
				}
			} else if coding.Load() != 3 || delivered != 1 || view.Status != "completed" {
				t.Fatal(coding.Load(), delivered, view)
			}
		})
	}
}

func TestChildSteerDeliveryReportFailureRollsBackAdmission(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	grant, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "fixture"}, func(context.Context, io.Writer, io.Writer) (jobs.TaskResult, error) { return jobs.TaskResult{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	actor := "main/child_report_failure"
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	assignment := &childAssignment{accepting: true, turn: turn, job: "job_fixture", queueTokenLimit: 1024}
	child := &codingChild{id: actor, turn: turn, job: assignment.job, assignment: assignment, state: "running", selection: r.selection, tools: r.Tools}
	r.children[actor] = child
	invalid := sendChildSteer(t, r, "main", "parent_call", map[string]any{"job_id": child.job, "message": "invalid", "job_read_grants": []string{grant, "missing"}})
	if invalid["ok"] == true || len(assignment.queued) != 0 {
		t.Fatal("invalid mixed grant was queued", invalid)
	}
	maxIDs := make([]string, maxJobReadGrants)
	for i := range maxIDs {
		maxIDs[i] = grant
	}
	oversized := append(append([]string(nil), maxIDs...), grant)
	invalid = sendChildSteer(t, r, "main", "parent_call", map[string]any{"job_id": child.job, "message": "invalid", "job_read_grants": oversized})
	if invalid["ok"] == true || !strings.Contains(fmt.Sprint(invalid), "at most 128 IDs") || len(assignment.queued) != 0 {
		t.Fatal("oversized grant was queued", invalid)
	}
	result := sendChildSteer(t, r, "main", "parent_call", map[string]any{"job_id": child.job, "message": "immutable parent evidence", "job_read_grants": maxIDs})
	if result["state"] != "queued" {
		t.Fatal(result)
	}
	if !strings.Contains(assignment.queued[0].Content, grant) || strings.Contains(assignment.queued[0].Content, "job_read_grant_notice") {
		t.Fatal("steering must identify grants without redundant prose", assignment.queued[0].Content)
	}
	projected, err := assignment.queued[0].ModelContent()
	const prefix = "<steer>\nimmutable parent evidence\n</steer>\n\n"
	if err != nil || !strings.HasPrefix(projected, prefix) {
		t.Fatal("steering text was not preserved before grant data", projected, err)
	}
	var notice struct {
		Type   string   `yaml:"type"`
		JobIDs []string `yaml:"job_ids"`
	}
	decodeSessionModelData(t, strings.TrimPrefix(projected, prefix), "runtime_notification", &notice)
	if notice.Type != "job_read_grants" || !reflect.DeepEqual(notice.JobIDs, []string{grant}) {
		t.Fatal("steering grant notice changed granted capabilities", notice)
	}
	queuedTokens := assignment.queuedTokens
	task := childTask{actor: actor, turn: turn, selection: r.selection, tools: child.tools, child: child, assignment: assignment, estimator: &contextbuild.Estimator{}}
	input := []llm.Message{{Type: llm.TypeUserInput, Content: "task", InputSource: "task"}}
	var before int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM model_requests WHERE actor_id=?`, actor).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER reject_child_message_report BEFORE INSERT ON entries
WHEN json_extract(NEW.content_json,'$.body.type')='child_message_delivered'
BEGIN SELECT RAISE(ABORT,'injected child delivery report failure'); END`); err != nil {
		t.Fatal(err)
	}
	admitted, cursor, err := r.admitChild(context.Background(), task, input, contextCursor{})
	if err == nil || !strings.Contains(err.Error(), "injected child delivery report failure") || admitted.RequestID != 0 || len(admitted.Notifications) != 0 || !reflect.DeepEqual(cursor, contextCursor{}) {
		t.Fatal(admitted, cursor, err)
	}
	if len(assignment.queued) != 1 || assignment.queuedTokens != queuedTokens || len(pendingMainNotices(t, r)) != 0 {
		t.Fatal("failed admission consumed or published queue")
	}
	if _, err := r.Jobs.Read(context.Background(), actor, grant, jobs.ReadOptions{Limit: 100}); err == nil {
		t.Fatal("failed admission granted access")
	}
	var after, delivered, messages int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM model_requests WHERE actor_id=?`, actor).Scan(&after); err != nil || after != before {
		t.Fatal(before, after, err)
	}
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message' AND delivered_request_id IS NOT NULL`).Scan(&delivered); err != nil || delivered != 0 {
		t.Fatal(delivered, err)
	}
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE actor_id=? AND kind='message'`, actor).Scan(&messages); err != nil || messages != 0 {
		t.Fatal(messages, err)
	}
	if _, err := historytest.DB(t, r.Store).Exec(`DROP TRIGGER reject_child_message_report`); err != nil {
		t.Fatal(err)
	}
	admitted, _, err = r.admitChild(context.Background(), task, input, contextCursor{})
	if err != nil || admitted.RequestID == 0 || len(admitted.Notifications) != 1 || len(pendingMainNotices(t, r)) != 1 || len(assignment.queued) != 0 || assignment.queuedTokens != 0 {
		t.Fatal(admitted, err)
	}
	if _, err := r.Jobs.Read(context.Background(), actor, grant, jobs.ReadOptions{Limit: 100}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(admitted.Messages[len(admitted.Messages)-1].Content, grant) {
		found := false
		for _, m := range admitted.Messages {
			if strings.Contains(m.Content, grant) {
				found = true
			}
		}
		if !found {
			t.Fatal("grant missing from delivered guidance")
		}
	}
	if err := r.Store.FinishRequest(admitted.RequestID, history.RequestCompletion{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
}

func TestChildSteerCumulativeQueueCapacity(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	assignment := &childAssignment{accepting: true, turn: "turn_fixture", job: "job_fixture", queueTokenLimit: 150}
	child := &codingChild{id: "main/child_capacity", turn: assignment.turn, job: assignment.job, assignment: assignment, state: "running", selection: r.selection}
	r.children[child.id] = child
	first := sendChildSteer(t, r, "main", "call", map[string]any{"job_id": child.job, "message": strings.Repeat("a", 180)})
	if first["state"] != "queued" {
		t.Fatal(first)
	}
	tokens := assignment.queuedTokens
	second := sendChildSteer(t, r, "main", "call", map[string]any{"job_id": child.job, "message": strings.Repeat("b", 180)})
	if second["ok"] == true || !strings.Contains(fmt.Sprint(second), "message_queue_capacity") || len(assignment.queued) != 1 || assignment.queuedTokens != tokens {
		t.Fatal(second, assignment)
	}
	var evidence int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message'`).Scan(&evidence); err != nil || evidence != 1 {
		t.Fatal(evidence, err)
	}
}
