package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/provider"
)

func TestDisposableChildDeliversBoundedFinalAnswerAndStopsOwnedJobs(t *testing.T) {
	answer := "Final answer:\n" + strings.Repeat("界", history.MaxChildAnswerBytes/3+100)
	r, _ := runtimeFixture(t, []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{"prompt":"answer with evidence","label":"audit","persistent":false}`)}}},
		{Text: "Intermediate commentary must not become the answer.", Calls: []provider.ToolCall{{ID: "shell", Name: "shell", Arguments: []byte(`{"command":"sleep 30","background":true,"wake_on_exit":false}`)}}},
		{Text: answer},
		{Text: "Parent used the delivered answer."},
	})
	r.Emit = nil
	m := provider.Message{Role: "user", Content: "Run a disposable audit"}
	if err := r.Run(&m); err != nil {
		t.Fatal(err)
	}
	if len(r.ChildViews("main")) != 0 || len(r.Jobs.Live()) != 0 || r.HasNotifications() {
		t.Fatal("disposable child or owned job still active")
	}
	if totals := r.UsageSnapshot().Totals; totals.Requests != 4 || totals.ReportedRequests != 4 {
		t.Fatal("child usage not accumulated exactly once", totals)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Answer     string `json:"answer"`
		Truncated  bool   `json:"answer_truncated"`
		Persistent bool   `json:"persistent"`
		Entry      int64  `json:"result_entry_id"`
		Stdout     *string
	}
	for _, message := range messages {
		if message.Role == "tool" && message.CallID == "child" {
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Contains(message.Content, "Intermediate commentary") {
			t.Fatal("intermediate child transcript leaked")
		}
	}
	if !result.Truncated || result.Persistent || result.Answer == "" || len(result.Answer) > history.MaxChildAnswerBytes || !utf8.ValidString(result.Answer) || !strings.HasPrefix(answer, result.Answer) || result.Stdout != nil {
		t.Fatal("invalid delivered answer or repeated stdout", result)
	}
	entry, err := r.Store.Entry(result.Entry)
	if err != nil {
		t.Fatal(err)
	}
	var original provider.Message
	if err := json.Unmarshal(entry.Content, &original); err != nil || original.Content != answer {
		t.Fatal("full immutable answer lost", err)
	}
	var shellStopped bool
	for _, job := range r.Jobs.List("main", true) {
		if job.Kind == "shell" {
			shellStopped = job.Status == "cancelled"
		}
	}
	if !shellStopped {
		t.Fatal("child-owned shell was not stopped")
	}
}

func TestBackgroundChildAnswerDeliveredOnceAcrossCompaction(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	r.selection.Model.Budget.RecentTokensMin = 0
	r.selection.Model.Budget.RecentTokensMax = 256
	seedCompactionHistory(t, r, strings.Repeat("Earlier research context. ", 100))
	started, release := make(chan struct{}), make(chan struct{})
	const answer = "The audit found one actionable issue."
	mainCalls, delivered := 0, 0
	r.Provider = &childProvider{stream: func(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
		text := "Parent is waiting for the child."
		if req.NoTools {
			text = "A disposable audit is running; consume its completion answer."
		} else if strings.Contains(req.System, "You are an isolated child agent.") {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			text = answer
		} else {
			mainCalls++
			if mainCalls == 1 {
				call := provider.ToolCall{ID: "background", Name: "subagent", Arguments: []byte(`{"prompt":"audit","label":"audit","persistent":false,"background":true}`)}
				if err := emit(provider.StreamEvent{Kind: "call", Call: &call}); err != nil {
					return err
				}
				text = ""
			}
			for _, m := range req.Messages {
				if m.Role != "user" || !m.Runtime {
					continue
				}
				var finish history.ChildFinish
				if json.Unmarshal([]byte(m.Content), &finish) == nil && finish.Type == "child_turn_finished" {
					if finish.Answer != answer || finish.Persistent || finish.Truncated || m.EventSeq == 0 {
						t.Error("invalid child completion", finish, m.EventSeq)
					}
					delivered++
					text = "Parent consumed the child answer directly."
				}
			}
		}
		if text != "" {
			if err := emit(provider.StreamEvent{Kind: "text", Text: text}); err != nil {
				return err
			}
		}
		return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 100, OutputTokens: 10}})
	}}
	m := provider.Message{Role: "user", Content: "Launch an audit"}
	if err := r.Run(&m); err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	views := r.ChildViews("main")
	if len(views) != 1 {
		t.Fatal(views)
	}
	if _, err := r.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.Jobs.Wait(ctx, "main", views[0].JobID, nil); err != nil {
		t.Fatal(err)
	}
	if !r.HasNotifications() {
		t.Fatal("missing background completion")
	}
	if err := r.Run(nil); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || mainCalls != 3 || r.HasNotifications() || len(r.ChildViews("main")) != 0 {
		t.Fatal("completion duplicated or required retrieval", delivered, mainCalls)
	}
	if totals := r.UsageSnapshot().Totals; totals.Requests != 5 || totals.ReportedRequests != 5 || totals.Tokens.InputTokens != 500 {
		t.Fatal("usage lost across compaction/child delivery", totals)
	}
}

func TestChildResultUsesCommittedCompletionBeforeJobSnapshot(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	child := &codingChild{id: "main/child"}
	for _, answer := range []string{"Final answer", ""} {
		assignment := &childAssignment{turn: "ct", finish: 1, result: 2, status: "completed", answer: answer}
		result := r.childResult(child, assignment, jobs.Snapshot{Status: "running", Stdout: "Intermediate commentary"})
		if result["status"] != "completed" || result["answer"] != answer || result["stdout"] != nil {
			t.Fatal("fast completion lost its answer or exposed commentary", result)
		}
		assignment.background = true
		for _, status := range []string{"running", "completed"} {
			result = r.childResult(child, assignment, jobs.Snapshot{Status: status, Stdout: "Intermediate commentary or final answer"})
			if result["answer"] != nil || result["stdout"] != nil || result["finish_event_seq"] != nil {
				t.Fatal("background launch duplicates completion", result)
			}
		}
	}
}

func TestChildCommitFailureStopsPersistentOwnedJobs(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "child", Name: "subagent", Arguments: []byte(`{"prompt":"audit","label":"audit","persistent":true}`)}}},
		{Calls: []provider.ToolCall{{ID: "shell", Name: "shell", Arguments: []byte(`{"command":"sleep 30","background":true,"wake_on_exit":false}`)}}},
		{Text: "Final audit answer"},
	})
	r.Emit = nil
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER reject_child_finish BEFORE INSERT ON entries
WHEN json_extract(NEW.content_json,'$.type')='child_turn_finished'
BEGIN SELECT RAISE(ABORT,'injected child commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	m := provider.Message{Role: "user", Content: "Audit"}
	err := r.Run(&m)
	if err == nil || !strings.Contains(err.Error(), "injected child commit failure") {
		t.Fatal("commit failure was not reported", err)
	}
	if len(r.ChildViews("main")) != 0 || len(r.Jobs.Live()) != 0 {
		t.Fatal("failed child commit leaked context or owned job")
	}
}
