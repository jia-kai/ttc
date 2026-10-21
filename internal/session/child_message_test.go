package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func sendChildMessage(t *testing.T, r *Runtime, actor, call string, args map[string]any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(args)
	record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: actor, CallID: call}, "child_message", data)
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestChildMessagesFIFOContinueSameAssignment(t *testing.T) {
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
		result := sendChildMessage(t, r, "main", ids[0], map[string]any{"child_id": first["child_id"], "child_turn_id": first["child_turn_id"], "message": text})
		if result["ok"] != true || result["state"] != "queued" || result["delivered"] != false {
			t.Fatal(result)
		}
	}
	if result := sendChildMessage(t, r, "main", ids[0], map[string]any{"child_id": first["child_id"], "child_turn_id": "stale", "message": "no"}); result["ok"] == true {
		t.Fatal(result)
	}
	if result := sendChildMessage(t, r, first["child_id"].(string), ids[0], map[string]any{"job_id": first["job_id"], "message": "no"}); result["ok"] == true {
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
	if result := sendChildMessage(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "too late"}); result["ok"] == true {
		t.Fatal(result)
	}
}

func TestChildMessageCancellationReportsUndelivered(t *testing.T) {
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
	result := sendChildMessage(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "original evidence"})
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

func TestChildMessageWaitsForForegroundBatch(t *testing.T) {
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
	result := sendChildMessage(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": "after tools"})
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

func TestChildMessageQueuedDuringCompaction(t *testing.T) {
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
			started, release := make(chan struct{}), make(chan struct{})
			var coding, summaries atomic.Int32
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.NoTools {
					summaries.Add(1)
					close(started)
					<-release
					return emit(llm.StreamEvent{Kind: "text", Text: "Concise child handoff."})
				}
				if coding.Add(1) == 1 {
					if err := emit(llm.StreamEvent{Kind: "text", Text: strings.Repeat("Old child research notes. ", 700)}); err != nil {
						return err
					}
					return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "noop", Name: "child_noop", Arguments: []byte(`{}`)}})
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
			result := sendChildMessage(t, r, "main", ids[0], map[string]any{"job_id": first["job_id"], "message": text})
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
				if coding.Load() != 2 || delivered != 0 || view.Status != "completed" {
					t.Fatal(coding.Load(), delivered, view)
				}
				var undelivered int
				if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.body.type')='child_message_undelivered'`).Scan(&undelivered); err != nil || undelivered != 0 {
					t.Fatal(undelivered, err)
				}
			} else if coding.Load() != 2 || delivered != 1 || view.Status != "completed" {
				t.Fatal(coding.Load(), delivered, view)
			}
		})
	}
}

func TestChildMessageDeliveryReportFailureRollsBackAdmission(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	actor := "main/child_report_failure"
	turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	assignment := &childAssignment{accepting: true, turn: turn, job: "job_fixture", queueTokenLimit: 1024}
	child := &codingChild{id: actor, turn: turn, job: assignment.job, assignment: assignment, state: "running", selection: r.selection, tools: r.Tools}
	r.children[actor] = child
	result := sendChildMessage(t, r, "main", "parent_call", map[string]any{"job_id": child.job, "message": "immutable parent evidence"})
	if result["state"] != "queued" {
		t.Fatal(result)
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
	if len(assignment.queued) != 1 || assignment.queuedTokens != queuedTokens || len(r.notifications) != 0 {
		t.Fatal("failed admission consumed or published queue")
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
	if err != nil || admitted.RequestID == 0 || len(admitted.Notifications) != 1 || len(r.notifications) != 1 || len(assignment.queued) != 0 || assignment.queuedTokens != 0 {
		t.Fatal(admitted, err)
	}
	if err := r.Store.FinishRequest(admitted.RequestID, "completed", nil); err != nil {
		t.Fatal(err)
	}
}

func TestChildMessageCumulativeQueueCapacity(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedRuntime(t, r, "parent")
	assignment := &childAssignment{accepting: true, turn: "turn_fixture", job: "job_fixture", queueTokenLimit: 200}
	child := &codingChild{id: "main/child_capacity", turn: assignment.turn, job: assignment.job, assignment: assignment, state: "running", selection: r.selection}
	r.children[child.id] = child
	first := sendChildMessage(t, r, "main", "call", map[string]any{"job_id": child.job, "message": strings.Repeat("a", 180)})
	if first["state"] != "queued" {
		t.Fatal(first)
	}
	tokens := assignment.queuedTokens
	second := sendChildMessage(t, r, "main", "call", map[string]any{"job_id": child.job, "message": strings.Repeat("b", 180)})
	if second["ok"] == true || !strings.Contains(fmt.Sprint(second), "message_queue_capacity") || len(assignment.queued) != 1 || assignment.queuedTokens != tokens {
		t.Fatal(second, assignment)
	}
	var evidence int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_message'`).Scan(&evidence); err != nil || evidence != 1 {
		t.Fatal(evidence, err)
	}
}
