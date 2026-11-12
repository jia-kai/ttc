package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/jobs"
	"ttc/internal/llm"
)

// pendingMainNotices reads complete sources selected for the next bounded
// delivery. Callers serialize with publishers when their fixture has live work.
func pendingMainNotices(t *testing.T, r *Runtime) []llm.Message {
	t.Helper()
	return pendingNoticeSources(t, r, &r.notifications)
}

func pendingChildNotices(t *testing.T, r *Runtime, assignment *childAssignment) []llm.Message {
	t.Helper()
	return pendingNoticeSources(t, r, &assignment.runtimeNotices)
}

func pendingNoticeSources(t *testing.T, r *Runtime, backlog *runtimeNoticeBacklog) []llm.Message {
	t.Helper()
	batch, _, err := backlog.batch(r.projectRuntimeNotice, 0)
	if err != nil {
		t.Fatal(err)
	}
	var messages []llm.Message
	for _, pending := range batch {
		message, err := r.projectRuntimeNotice(pending.EventSeq, 0)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	return messages
}

func queueMainBurst(t *testing.T, r *Runtime, count int) []int64 {
	t.Helper()
	var sources []int64
	for i := range count {
		body := jobExitContent(jobs.Snapshot{ID: fmt.Sprintf("job_%d", i), Kind: "shell", Status: "completed", Stdout: strings.Repeat("x", 8192)})
		if err := r.queueNotification(body); err != nil {
			t.Fatal(err)
		}
		saved, err := r.Store.Session(r.Current())
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, saved.EntryTip)
	}
	return sources
}

func assertMainAcknowledgments(t *testing.T, r *Runtime, sources []int64) {
	t.Helper()
	rows, err := historytest.DB(t, r.Store).Query(`SELECT delivered_events_json FROM model_requests WHERE actor_id='main' AND purpose='coding' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actual []int64
	batches := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var batch []int64
		if err := json.Unmarshal([]byte(raw), &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch) > maxRuntimeNoticeBatch {
			t.Fatal("unbounded admission", batch)
		}
		if len(batch) > 0 {
			batches++
		}
		actual = append(actual, batch...)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, sources) || batches < 2 || r.HasNotifications() {
		t.Fatalf("source admission must be ordered, exactly once, and fully drained: got=%v want=%v batches=%d", actual, sources, batches)
	}
	for _, seq := range sources {
		var count int
		if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE kind='message' AND source_id IS NULL AND json_extract(content_json,'$.event_seq')=?`, seq).Scan(&count); err != nil || count != 1 {
			t.Fatalf("source %d has %d admitted messages: %v", seq, count, err)
		}
	}
}

func TestMainNotificationBurstDrainsBoundedBatches(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	seedCompactionHistory(t, r, strings.Repeat("Older completed evidence. ", 500))
	sources := queueMainBurst(t, r, 80)
	// The regression is specifically larger than a complete coding request.
	var full []llm.Message
	for _, source := range sources {
		message, err := r.projectRuntimeNotice(source, 0)
		if err != nil {
			t.Fatal(err)
		}
		full = append(full, message)
	}
	if contextbuild.Tokens(full) <= r.selection.Model.Budget.ContextLimit {
		t.Fatal("fixture burst fits model budget")
	}
	batch, count, err := r.notifications.batch(r.projectRuntimeNotice, 0)
	if err != nil || count == 0 || count >= len(sources) || count > 1 && contextbuild.Tokens(batch) > maxRuntimeNoticeTokens {
		t.Fatal("invalid bounded projection", count, err)
	}
	for _, message := range batch {
		text, err := message.ModelContent()
		if err != nil || !strings.Contains(message.Content, `"output_omitted":true`) || !strings.Contains(message.Content, strings.Repeat("x", 100)) || len(text) > maxRuntimeNoticeBytes {
			t.Fatal("large output lost bounded prefix or durable reference", message, err)
		}
	}
	steer := strings.Repeat("Human instruction remains exact. ", 100)
	r.steers = []contextbuild.Input{{Text: steer}}
	seenSteer := false
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		for _, message := range req.Messages {
			seenSteer = seenSteer || message.Type == llm.TypeUserInput && message.Content == steer
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Processed events."})
	}}
	if _, err := r.Run(nil); err != nil {
		t.Fatal(err)
	}
	if !seenSteer {
		t.Fatal("human steer was compressed or omitted")
	}
	assertMainAcknowledgments(t, r, sources)
	for _, seq := range sources {
		message, err := r.projectRuntimeNotice(seq, 0)
		if err != nil || !strings.Contains(message.Content, strings.Repeat("x", 8192)) {
			t.Fatal("batching changed durable payload", seq, err)
		}
	}
}

func TestMainBacklogArrivalsDuringCompactionAndRecovery(t *testing.T) {
	for _, recoverBoundary := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%v", recoverBoundary), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			seedCompactionHistory(t, r, strings.Repeat("Earlier completed evidence. ", 500))
			sourceSession := r.Current()
			sources := queueMainBurst(t, r, 40)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if !req.NoTools {
					t.Fatal("expected summary")
				}
				sources = append(sources, queueMainBurst(t, r, 40)...)
				if recoverBoundary {
					cancel()
					return ctx.Err()
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Earlier work summarized."})
			}}
			_, err := r.compactContext(ctx, "", r.selection, nil, false)
			if recoverBoundary {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if err := r.Load(sourceSession); err != nil {
					t.Fatal(err)
				}
				recovered, err := r.Store.PendingRecoveryNotifications(context.Background(), r.Current())
				if err != nil || len(recovered) != len(sources) {
					t.Fatal("boundary recovery lost pending references")
				}
				batch, _, err := r.notifications.batch(r.projectRuntimeNotice, 0)
				if err != nil {
					t.Fatal(err)
				}
				for i, message := range batch {
					var ref struct {
						Recovered int64 `json:"recovered_from_event_seq"`
						Omitted   bool  `json:"output_omitted"`
					}
					if err := json.Unmarshal([]byte(message.Content), &ref); err != nil || !ref.Omitted || ref.Recovered != sources[i] {
						t.Fatal("bounded reference lost recovery provenance", message, err)
					}
				}
				for i, event := range recovered {
					message, err := r.projectRuntimeNotice(event.EventSeq, 0)
					if err != nil {
						t.Fatal(err)
					}
					var provenance struct {
						Recovered int64 `json:"recovered_from_event_seq"`
					}
					if err := json.Unmarshal([]byte(message.Content), &provenance); err != nil || provenance.Recovered != sources[i] || message.EventSeq == sources[i] {
						t.Fatal("recovery reused ownership or lost provenance", message, err)
					}
					sources[i] = message.EventSeq
				}
			} else if err != nil || r.Current() == sourceSession || !r.HasNotifications() {
				t.Fatal("compaction dropped or tried to admit entire burst", err)
			}
			r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				return emit(llm.StreamEvent{Kind: "text", Text: "Processed remaining evidence."})
			}}
			if _, err := r.Run(nil); err != nil {
				t.Fatal(err)
			}
			assertMainAcknowledgments(t, r, sources)
		})
	}
}

func TestMainBacklogCoalescesTimerAtLatestSourcePosition(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe events")
	for _, body := range []string{`{"type":"wakeup","wakeup_id":"timer","fired_count":1}`, `{"type":"job_exit","job_id":"between"}`, `{"type":"wakeup","wakeup_id":"timer","fired_count":2}`} {
		if err := r.queueNotification(body); err != nil {
			t.Fatal(err)
		}
	}
	batch, count, err := r.notifications.batch(r.projectRuntimeNotice, 0)
	if err != nil || count != 2 || batch[0].EventSeq >= batch[1].EventSeq || !strings.Contains(batch[0].Content, "between") || !strings.Contains(batch[1].Content, `"fired_count":2`) {
		t.Fatal("timer replacement reordered pending source events", batch, err)
	}
	admitted, _, err := r.admitMain(context.Background(), admissionTurn(t, r), r.selection)
	if err != nil || len(admitted.NoticeEntries) != 2 {
		t.Fatal(admitted, err)
	}
}

func TestMainOversizedChildCompletionKeepsActionableReference(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe child")
	turn, err := r.Store.BeginChildTurn(r.Current(), "main/child_large", r.selection)
	if err != nil {
		t.Fatal(err)
	}
	answer := strings.Repeat("Large evidence. ", 380)
	result, err := r.Store.Append(r.Current(), turn, "main/child_large", "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: answer})
	if err != nil {
		t.Fatal(err)
	}
	finish := history.ChildFinish{ChildID: "main/child_large", TurnID: turn, JobID: "child_job", Outcome: "failed", ContextState: "closed", ResultEntry: result, Answer: answer, Warning: "Partial evidence only", TranscriptExportError: "Fixture export unavailable"}
	seq, err := r.Store.FinishChildTurn(r.Current(), finish)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.queueCommittedNotification(seq); err != nil {
		t.Fatal(err)
	}
	batch, count, err := r.notifications.batch(r.projectRuntimeNotice, 0)
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var notice struct {
		childNotification
		Omitted bool `json:"output_omitted"`
	}
	if err := json.Unmarshal([]byte(batch[0].Content), &notice); err != nil {
		t.Fatal(err)
	}
	if !notice.Omitted || notice.ChildID != finish.ChildID || notice.TurnID != turn || notice.JobID != finish.JobID || notice.AssignmentStatus != "failed" || notice.ChildState != "closed" || notice.ResultEntry == nil || *notice.ResultEntry != result || batch[0].EventSeq != seq {
		t.Fatal("reference lost completion identity/outcome", batch[0])
	}
	admitted, _, err := r.admitMain(context.Background(), admissionTurn(t, r), r.selection)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range admitted.Messages {
		if message.EventSeq == seq && strings.Contains(message.Content, answer) {
			t.Fatal("full UI answer entered model request")
		}
	}
	var presentation string
	r.Emit = func(event Event) {
		if event.Kind == "message" && event.ChildCompletion != nil {
			presentation = event.ChildCompletion.Answer
			if strings.Contains(event.Text, answer) {
				t.Fatal("human presentation changed canonical event text")
			}
		}
	}
	if err := r.deliverNoticeEntries("main", admitted.NoticeEntries); err != nil || !strings.Contains(presentation, answer) {
		t.Fatal("live notification lost canonical answer preview", err)
	}
}

func TestMainCommittedNotificationInsertedBySourceOrder(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe delayed publication")
	body := `{"type":"job_exit","job_id":"earlier"}`
	earlier, err := r.Store.Append(r.Current(), "", "main", "status", "", false, map[string]any{"type": "runtime_event", "body": json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.queueNotification(`{"type":"job_exit","job_id":"later"}`); err != nil {
		t.Fatal(err)
	}
	if err := r.queueCommittedNotification(earlier); err != nil {
		t.Fatal(err)
	}
	batch, count, err := r.notifications.batch(r.projectRuntimeNotice, 0)
	if err != nil || count != 2 || batch[0].EventSeq != earlier || batch[1].EventSeq <= earlier {
		t.Fatal("late publication changed durable source ordering", batch, err)
	}
}

func TestOmittedChildCompletionRemainsRecoverableThroughMainExport(t *testing.T) {
	for _, hasResult := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed_answer=%v", hasResult), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			seedRuntime(t, r, "Observe child")
			child := "main/child_export"
			turn, err := r.Store.BeginChildTurn(r.Current(), child, r.selection)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Store.Append(r.Current(), turn, child, "message", "user", false, llm.Message{Type: llm.TypeUserInput, Content: "PRIVATE_CHILD_TASK_NOT_RECEIVED_BY_MAIN"}); err != nil {
				t.Fatal(err)
			}
			answer := "UNIQUE_CHILD_ANSWER " + strings.Repeat("large evidence ", 400)
			var result int64
			if hasResult {
				result, err = r.Store.Append(r.Current(), turn, child, "message", "assistant", false, llm.Message{Type: llm.TypeAssistant, Content: answer})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "child-failure.md")
			finish := history.ChildFinish{ChildID: child, TurnID: turn, JobID: "job_export", Outcome: "failed", ContextState: "closed", ResultEntry: result, Answer: answer, Warning: "UNIQUE_FAILURE_WARNING", Error: "UNIQUE_FAILURE_ERROR", TranscriptPath: path, TranscriptJSONLPath: path + ".jsonl"}
			seq, err := r.Store.FinishChildTurn(r.Current(), finish)
			if err != nil {
				t.Fatal(err)
			}
			before, err := r.Store.ActorTranscriptJSONL(r.Current(), "main", seq)
			if err != nil || bytes.Contains(before, []byte(finish.Warning)) {
				t.Fatal("undelivered source escaped frozen actor selection", err)
			}
			if err := r.queueCommittedNotification(seq); err != nil {
				t.Fatal(err)
			}
			admitted, _, err := r.admitMain(context.Background(), admissionTurn(t, r), r.selection)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range admitted.Messages {
				if message.EventSeq == seq && (!strings.Contains(message.Content, `"output_omitted":true`) || !strings.Contains(message.Content, finish.Warning)) {
					t.Fatal("export fixture must keep model input bounded", message)
				}
			}
			tip, err := r.Store.SessionTip(context.Background(), r.Current())
			if err != nil {
				t.Fatal(err)
			}
			for _, format := range []string{"jsonl", "markdown"} {
				var out bytes.Buffer
				if err := r.Store.WriteActorTranscript(context.Background(), &out, r.Current(), "main", tip, format); err != nil {
					t.Fatal(err)
				}
				for _, required := range []string{"UNIQUE_CHILD_ANSWER", finish.Warning, finish.Error, finish.TranscriptPath, finish.TranscriptJSONLPath} {
					if !strings.Contains(out.String(), required) {
						t.Fatalf("%s cannot recover omitted completion field %q", format, required)
					}
				}
				if strings.Contains(out.String(), "PRIVATE_CHILD_TASK_NOT_RECEIVED_BY_MAIN") {
					t.Fatal("source closure exported unrelated child conversation")
				}
			}
		})
	}
}

func TestChildDeliveryReceiptWakesMainBeforeAndDuringAwait(t *testing.T) {
	for _, alreadyPending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%v", alreadyPending), func(t *testing.T) {
			r, events := runtimeFixture(t, nil)
			started, release, delivered := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
				if requests.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					return emit(llm.StreamEvent{Kind: "text", Text: "First answer"})
				}
				close(delivered)
				<-ctx.Done()
				return ctx.Err()
			}}
			args := `{"persistent":false,"prompt":"task","label":"delivery probe","background":true}`
			_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
			child := childInvocation(t, r, ids[0], args)
			receive(t, started)
			result := sendChildSteer(t, r, "main", ids[0], map[string]any{"job_id": child["job_id"], "message": "Continue work"})
			if result["state"] != "queued" {
				t.Fatal(result)
			}
			if alreadyPending {
				close(release)
				receive(t, delivered)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := r.awaitActor(ctx, "main", time.Time{}); done <- err }()
			if !alreadyPending {
				waitForAwait(t, events, "main")
				close(release)
				receive(t, delivered)
			}
			if err := <-done; err != nil {
				t.Fatal("committed delivery receipt did not wake main", err)
			}
			r.orderMu.Lock()
			notices := pendingMainNotices(t, r)
			r.orderMu.Unlock()
			if len(notices) != 1 || !strings.Contains(notices[0].Content, "child_steer_delivered") {
				t.Fatal(notices)
			}
			var count int
			if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE json_extract(content_json,'$.body.type')='child_steer_delivered'`).Scan(&count); err != nil || count != 1 {
				t.Fatal("duplicated durable receipt", count, err)
			}
			if _, err := r.StopChild(context.Background(), "main", child["child_id"].(string)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
