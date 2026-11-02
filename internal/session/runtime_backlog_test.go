package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestChildRuntimeBacklogBatchesAndAcknowledgesEverySource(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	step := 0
	r.Provider = &childProvider{stream: func(ctx context.Context, request llm.Request, emit func(llm.StreamEvent) error) error {
		step++
		if step == 1 {
			r.orderMu.Lock()
			for i := range 40 {
				event := map[string]any{"type": "job_exit", "job_id": fmt.Sprintf("job_%d", i), "status": "completed", "stdout": "small"}
				if i == 0 {
					event["stdout"] = strings.Repeat("\x00", 8192)
					event["kind"] = "watch"
					event["exit_code"] = 7
					event["signal"] = "SIGTERM"
				}
				body, _ := json.Marshal(event)
				if err := r.queueActorNotificationLocked(request.ConversationID, string(body)); err != nil {
					r.orderMu.Unlock()
					return err
				}
			}
			assignment := r.actorAssignments[request.ConversationID]
			if len(assignment.queued) != 0 || assignment.queuedTokens != 0 {
				t.Error("runtime activity consumed parent guidance budget")
			}
			r.orderMu.Unlock()
			return emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: "await", Name: "await", Arguments: []byte(`{}`)}})
		}
		// The oversized first source must become a compact, individually acknowledged
		// reference, not an impossible admission/compaction loop.
		if step == 2 {
			found := false
			for _, message := range request.Messages {
				if message.Type != llm.TypeRuntimeNotification || !strings.Contains(message.Content, `"job_id":"job_0"`) {
					continue
				}
				found = strings.Contains(message.Content, `"output_omitted":true`)
				var outcome struct {
					Kind     string `json:"kind"`
					ExitCode *int   `json:"exit_code"`
					Signal   string `json:"signal"`
				}
				if err := json.Unmarshal([]byte(message.Content), &outcome); err != nil || outcome.Kind != "watch" || outcome.ExitCode == nil || *outcome.ExitCode != 7 || outcome.Signal != "SIGTERM" {
					t.Error("compact reference lost workload outcome", message.Content, err)
				}
			}
			if !found {
				t.Error("oversized source did not become a compact reference")
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Batch received"})
	}}
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":false,"prompt":"drain events","label":"backlog child"}`)}})
	result := childInvocation(t, r, ids[0], `{"persistent":false,"prompt":"drain events","label":"backlog child"}`)
	if result["ok"] != true || result["assignment_status"] != "completed" {
		t.Fatal(result)
	}
	if step < 3 {
		t.Fatal("backlog was not admitted in bounded batches", step)
	}
	var sources, acknowledged int
	err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*),count(delivered_request_id) FROM entries WHERE actor_id=? AND json_extract(content_json,'$.type')='runtime_event'`, result["child_id"]).Scan(&sources, &acknowledged)
	if err != nil || sources != 40 || acknowledged != sources {
		t.Fatal("source acknowledgments were lost", sources, acknowledged, err)
	}
}

func noticeSequences(messages []llm.Message) []int64 {
	var sources []int64
	for _, message := range messages {
		if message.Type == llm.TypeRuntimeNotification {
			sources = append(sources, message.EventSeq)
		}
	}
	return sources
}

func TestRuntimeNoticeBacklogContract(t *testing.T) {
	var q runtimeNoticeBacklog
	project := func(seq int64, tokenLimit int) (llm.Message, error) {
		if tokenLimit <= 0 || tokenLimit > maxRuntimeNoticeTokens {
			t.Fatalf("unbounded delivery budget: %d", tokenLimit)
		}
		return llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: seq, Content: `{}`}, nil
	}
	enqueue := func(seq int64, timer string) {
		t.Helper()
		if err := q.enqueue(seq, timer); err != nil {
			t.Fatal(err)
		}
	}
	assertBatch := func(want ...int64) int {
		t.Helper()
		batch, count, err := q.batch(project, 0)
		if err != nil || count != len(want) || !reflect.DeepEqual(noticeSequences(batch), want) {
			t.Fatalf("batch=%v count=%d want=%v error=%v", noticeSequences(batch), count, want, err)
		}
		return count
	}
	assertBatch()
	enqueue(2, "timer")
	enqueue(5, "")
	enqueue(3, "") // A previously committed completion arrives late.
	enqueue(6, "timer")
	enqueue(1, "timer") // A stale firing cannot replace the newest source.
	assertBatch(3, 5, 6)
	for _, source := range []int64{3, 5, 6} {
		if err := q.enqueue(source, "other"); err == nil {
			t.Fatal("duplicate source accepted", source)
		}
	}
	if err := q.enqueue(0, ""); err == nil {
		t.Fatal("invalid source accepted")
	}
	failed := errors.New("projection failed")
	batch, count, err := q.batch(func(seq int64, limit int) (llm.Message, error) {
		if seq == 5 {
			return llm.Message{}, failed
		}
		return project(seq, limit)
	}, 0)
	if !errors.Is(err, failed) || batch != nil || count != 0 {
		t.Fatal("partial failed projection escaped", batch, count, err)
	}
	assertBatch(3, 5, 6) // Selection and failure consume nothing.
	q.consume(2)
	assertBatch(6)
	q.consume(1)
	assertBatch()
	enqueue(7, "timer") // Consumption also releases the timer's coalescing slot.
	assertBatch(7)
	q.consume(1)

	for seq := int64(1); seq <= maxRuntimeNoticeBatch+5; seq++ {
		enqueue(seq, "")
	}
	batch, count, err = q.batch(project, 0)
	if err != nil || count != maxRuntimeNoticeBatch || len(batch) != count || batch[0].EventSeq != 1 || batch[count-1].EventSeq != int64(count) {
		t.Fatal("count bound/order", batch, count, err)
	}
	q.consume(count)
	assertBatch(33, 34, 35, 36, 37)
	batch, count, err = q.batch(project, 1)
	if err != nil || count != 1 || batch[0].EventSeq != 33 {
		t.Fatal("small budgets must make progress", batch, count, err)
	}
}

func TestRuntimeNoticeBacklogTokenBound(t *testing.T) {
	var q runtimeNoticeBacklog
	for seq := int64(1); seq <= 20; seq++ {
		if err := q.enqueue(seq, ""); err != nil {
			t.Fatal(err)
		}
	}
	project := func(seq int64, limit int) (llm.Message, error) {
		return llm.Message{Type: llm.TypeRuntimeNotification, EventSeq: seq, Content: fmt.Sprintf(`{"type":"job_exit","job_id":"%d","output":"%s"}`, seq, strings.Repeat("x", 200))}, nil
	}
	batch, count, err := q.batch(project, 500)
	if err != nil || count == 0 || count >= 20 || contextbuild.Tokens(batch) > 500 {
		t.Fatal("token limit not enforced", count, contextbuild.Tokens(batch), err)
	}
	if q.len() != 20 {
		t.Fatal("selection consumed pending input")
	}
	q.consume(count)
	next, _, err := q.batch(project, 500)
	if err != nil || next[0].EventSeq != int64(count+1) {
		t.Fatal("consumption did not retain the exact suffix", next, err)
	}
}

func TestActorsShareNoticeOrderingAndSuccessfulConsumption(t *testing.T) {
	for _, role := range []string{"main", "child"} {
		t.Run(role, func(t *testing.T) {
			a := newSummaryBudgetActor(t, role)
			actor := "main"
			if a.task != nil {
				actor = a.task.actor
			}
			publish := func(body string) int64 {
				t.Helper()
				a.r.orderMu.Lock()
				err := a.r.queueActorNotificationLocked(actor, body)
				a.r.orderMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				saved, err := a.r.Store.Session(a.r.Current())
				if err != nil {
					t.Fatal(err)
				}
				return saved.EntryTip
			}
			publish(`{"type":"wakeup","wakeup_id":"timer","fired_count":1}`)
			// Delay publication of an already committed source until after newer
			// sources enter the backlog, just as completion handoff can do.
			earlier, err := a.r.Store.Append(a.r.Current(), "", actor, "status", "", false, map[string]any{"type": "runtime_event", "body": map[string]string{"type": "job_exit", "job_id": "earlier"}})
			if err != nil {
				t.Fatal(err)
			}
			var want []int64
			want = append(want, earlier)
			for i := range maxRuntimeNoticeBatch + 2 {
				want = append(want, publish(fmt.Sprintf(`{"type":"job_exit","job_id":"job_%d"}`, i)))
			}
			latest := publish(`{"type":"wakeup","wakeup_id":"timer","fired_count":2}`)
			want = append(want, latest)
			if a.task == nil {
				err = a.r.queueCommittedNotification(earlier)
			} else {
				err = a.task.assignment.runtimeNotices.enqueue(earlier, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := historytest.DB(t, a.r.Store).Exec(`CREATE TRIGGER refuse_backlog_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'refuse backlog admission'); END`); err != nil {
				t.Fatal(err)
			}
			if admission, err := a.admit(); err == nil || admission.RequestID != 0 {
				t.Fatal("injected admission failure did not fail", admission, err)
			}
			if reason, err := a.r.awaitActor(context.Background(), actor, time.Time{}); err != nil || reason != "pending_input" {
				t.Fatal("failed admission lost await eligibility", reason, err)
			}
			if _, err := historytest.DB(t, a.r.Store).Exec(`DROP TRIGGER refuse_backlog_admission`); err != nil {
				t.Fatal(err)
			}
			var got []int64
			for len(got) < len(want) {
				admission, err := a.admit()
				if err != nil {
					t.Fatal(err)
				}
				var raw string
				if err := historytest.DB(t, a.r.Store).QueryRow(`SELECT delivered_events_json FROM model_requests WHERE id=?`, admission.RequestID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var sources []int64
				if err := json.Unmarshal([]byte(raw), &sources); err != nil {
					t.Fatal(err)
				}
				if len(sources) == 0 || len(sources) > maxRuntimeNoticeBatch {
					t.Fatal("admission did not make bounded progress", sources)
				}
				got = append(got, sources...)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("admission source order/exactly-once mismatch: got=%v want=%v", got, want)
			}
			admission, err := a.admit()
			if err != nil || len(admission.NoticeEntries) != 0 {
				t.Fatal("successful admissions left pending duplicates", admission, err)
			}
		})
	}
}
