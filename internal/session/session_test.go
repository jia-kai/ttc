package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/scratch"
	"scicode/internal/skills"
	"scicode/internal/tool"
	"scicode/internal/workspace"
	"strings"
	"testing"
	"time"
)

func runtimeFixture(t *testing.T, responses []provider.ScriptResponse) (*Runtime, chan Event) {
	t.Helper()
	scratch.Verify()
	store, e := history.Open(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	w, e := workspace.Open(t.TempDir(), store)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { w.Close() })
	model := provider.ScriptModel()
	selection := provider.Selection{Provider: "script", Model: model, Variant: "none"}
	catalog, e := skills.Discover(context.Background(), w.Root, "")
	if e != nil {
		t.Fatal(e)
	}
	events := make(chan Event, 256)
	r := New(context.Background(), store, w, &provider.Script{Responses: responses}, selection, "", catalog, func(e Event) { events <- e })
	r.AutoName = false
	t.Cleanup(r.Close)
	return r, events
}

func seedRuntime(t *testing.T, r *Runtime, content string) {
	t.Helper()
	turn, _, err := r.Store.StartSession(r.Current(), r.Workspace.Root, r.CurrentSelection(), provider.Message{Role: "user", Content: content})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.persisted = true
	r.mu.Unlock()
}
func TestTurnToolRoundTripUndoRedoAndSystemInspection(t *testing.T) {
	r, events := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "p", Name: "write", Arguments: []byte(`{"path":"result.txt","content":"verified\n"}`)}}}, {Text: "Saved result."}})
	m := provider.Message{Role: "user", Content: "Write a result"}
	if e := r.Run(&m); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(r.Workspace.Root, "result.txt"))
	if e != nil || string(b) != "verified\n" {
		t.Fatal(string(b), e)
	}
	messages, e := r.Store.Messages(r.Current())
	if e != nil || len(messages) != 5 || messages[3].Role != "tool" {
		t.Fatal(messages, e)
	}
	prompts := 0
	for len(events) > 0 {
		event := <-events
		if event.Kind == "system_prompt" {
			prompts++
			entry, e := r.Store.Entry(event.EntryID)
			if e != nil {
				t.Fatal(e)
			}
			text, e := r.Store.Inspect(entry)
			if e != nil || !strings.Contains(text, "You are TTC") {
				t.Fatal(text, e)
			}
		}
	}
	if prompts != 2 {
		t.Fatal(prompts)
	}
	if _, e = r.Command("/undo"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(r.Workspace.Root, "result.txt")); !os.IsNotExist(e) {
		t.Fatal("undo left result")
	}
	if _, e = r.Command("/redo"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(r.Workspace.Root, "result.txt")); e != nil {
		t.Fatal(e)
	}
}
func TestQuestionAnswerAndPendingCancellation(t *testing.T) {
	r, events := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "q", Name: "question", Arguments: []byte(`{"questions":[{"id":"color","prompt":"Color?","options":[{"id":"red","label":"Red"},{"id":"blue","label":"Blue"}]}]}`)}}}, {Text: "Red selected."}})
	done := make(chan error, 1)
	go func() { m := provider.Message{Role: "user", Content: "Ask"}; done <- r.Run(&m) }()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Kind != "question" {
				continue
			}
			id := strings.Split(strings.Split(event.Text, " · ")[1], "\n")[0]
			if e := r.AnswerQuestion(id, []Answer{{ID: "color", Values: []string{"missing"}, Source: "option"}}); e == nil {
				t.Fatal("invalid option accepted")
			}
			if e := r.AnswerQuestion(id, []Answer{{ID: "color", Values: []string{"red"}, Source: "option"}}); e != nil {
				t.Fatal(e)
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			return
		case <-deadline:
			t.Fatal("question did not arrive")
		}
	}
}
func TestTimerCoalescingAndSwitchStopsJobs(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Schedule research reminders")
	wake, e := r.timers.schedule("check", "task", time.Now(), 1)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(time.Second)
	for !r.HasNotifications() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.HasNotifications() {
		t.Fatal("timer did not deliver")
	}
	if _, _, e = r.admitMain(context.Background(), "", r.CurrentSelection()); e != nil {
		t.Fatal(e)
	}
	r.timers.deliveredThrough(wake.ID, 1)
	listed := r.timers.list()
	if len(listed) != 1 || listed[0].ID != wake.ID || listed[0].Last != "delivered" {
		t.Fatal(listed)
	}
	id, e := r.Jobs.Start("main", "sleep 20", r.Workspace.Root, 0, true, true, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Command("/new"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Jobs.View("main", id); e == nil {
		t.Fatal("old job handle revived")
	}
	if len(r.timers.list()) != 0 {
		t.Fatal("timer revived")
	}
}
func TestPartialFailureLeavesUndoableAndWellFormedHistory(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "p", Name: "write", Arguments: []byte(`{"path":"x","content":"data"}`)}}}})
	m := provider.Message{Role: "user", Content: "write"}
	if e := r.Run(&m); e == nil {
		t.Fatal("expected script exhaustion")
	}
	if _, e := r.Command("/undo"); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(r.Workspace.Root, "x")); !os.IsNotExist(e) {
		t.Fatal("failed turn edit was not undoable")
	}
	var statuses []string
	rows, e := r.Store.DB.Query("SELECT status FROM turns")
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		rows.Scan(&status)
		statuses = append(statuses, status)
	}
	b, _ := json.Marshal(statuses)
	if string(b) != `["failed"]` {
		t.Fatal(string(b))
	}
}

type gatedNamingProvider struct {
	naming  chan provider.Request
	release chan struct{}
}

func (p *gatedNamingProvider) Models(context.Context) ([]provider.ModelSpec, error) {
	return []provider.ModelSpec{provider.ScriptModel()}, nil
}
func (p *gatedNamingProvider) Login(context.Context, provider.LoginUI) error { return nil }
func (p *gatedNamingProvider) EstimateReplay(m provider.Message) int {
	return provider.ReplayTokens(m.State)
}
func (p *gatedNamingProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if req.NoTools {
		p.naming <- req
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if e := emit(provider.StreamEvent{Kind: "text", Text: "Small Test Session"}); e != nil {
			return e
		}
	} else {
		if e := emit(provider.StreamEvent{Kind: "text", Text: "Completed"}); e != nil {
			return e
		}
	}
	return emit(provider.StreamEvent{Kind: "completed", Usage: &provider.Usage{InputTokens: 10, OutputTokens: 3}})
}
func TestNamingClaimIsFirstTurnAndDoesNotBlockNextTurn(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	p := &gatedNamingProvider{naming: make(chan provider.Request, 2), release: make(chan struct{})}
	r.Provider = p
	r.AutoName = true
	m := provider.Message{Role: "user", Content: "first prompt"}
	if e := r.Run(&m); e != nil {
		t.Fatal(e)
	}
	var firstTurn string
	if e := r.Store.DB.QueryRow("SELECT json_extract(metadata_json,'$.naming_turn_id') FROM sessions WHERE id=?", r.Current()).Scan(&firstTurn); e != nil || firstTurn == "" {
		t.Fatal(firstTurn, e)
	}
	select {
	case request := <-p.naming:
		if request.ConversationID != r.Current()+"/naming" {
			t.Fatal("incorrect naming identity", request.ConversationID)
		}
		if request.MaxAttempts != 1 {
			t.Fatal("naming may retry")
		}
	case <-time.After(time.Second):
		t.Fatal("naming not started")
	}
	done := make(chan error, 1)
	go func() { m := provider.Message{Role: "user", Content: "second prompt"}; done <- r.Run(&m) }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("naming blocked next turn")
	}
	close(p.release)
	<-r.namingDone
	v, e := r.Store.Session(r.Current())
	if e != nil || v.Name != "Small Test Session" {
		t.Fatal(v, e)
	}
	select {
	case <-p.naming:
		t.Fatal("second naming request")
	default:
	}
}

func TestBackgroundWithoutWakeStillPersistsOutput(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Start a background research command")
	id, e := r.Jobs.Start("main", "printf retained-output", r.Workspace.Root, 0, true, true, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Jobs.Wait(context.Background(), "main", id, nil); e != nil {
		t.Fatal(e)
	}
	r.Jobs.Close()
	if r.HasNotifications() {
		t.Fatal("wake_on_exit=false woke model")
	}
	entries, e := r.Store.Branch(r.Current(), 0)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range entries {
		if strings.Contains(string(v.Content), "retained-output") {
			text, e := r.Store.Inspect(v)
			if e != nil || !strings.Contains(text, "retained-output") {
				t.Fatal(text, e)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("background output lost from history")
	}
}

func TestEarlyBackgroundCompletionHasIndependentDurableCard(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "early", Name: "shell", Arguments: []byte(`{}`)}}}, {Text: "launched"}})
	completed := make(chan struct{})
	r.Emit = func(e Event) {
		if e.Kind == "job" {
			close(completed)
		}
	}
	r.Tools = tool.NewRegistry()
	tool.Register(r.Tools, "shell", "gated background launch", map[string]any{}, nil, func(struct{}) error { return nil }, func(ctx context.Context, x tool.Execution, _ struct{}) (any, error) {
		release := make(chan struct{})
		id, err := r.Jobs.StartTask("main", "shell", "printf output", true, false, func(ctx context.Context, stdout, stderr io.Writer) error {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			for i := 1; i <= 20; i++ {
				if _, err := fmt.Fprintf(stdout, "line-%02d\n", i); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		initial, err := r.Jobs.View("main", id)
		if err != nil {
			return nil, err
		}
		close(release)
		if _, err := r.Jobs.Wait(ctx, "main", id, nil); err != nil {
			return nil, err
		}
		select {
		case <-completed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return initial, nil // Preserve the running launch snapshot after completion.
	})
	if err := r.Run(&provider.Message{Role: "user", Content: "background"}); err != nil {
		t.Fatal(err)
	}
	if r.HasNotifications() {
		t.Fatal("wake disabled still notified model")
	}
	var completionID, resultID int64
	if err := r.Store.DB.QueryRow("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='job_completion'").Scan(&completionID); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT entry_id FROM tool_records").Scan(&resultID); err != nil {
		t.Fatal(err)
	}
	if completionID >= resultID {
		t.Fatal("completion was not before launch result")
	}
	entry, err := r.Store.Entry(completionID)
	if err != nil {
		t.Fatal(err)
	}
	if label := r.Store.Label(entry); !strings.Contains(label, "done") || strings.Contains(label, "line-01") || !strings.Contains(label, "line-20") {
		t.Fatal(label)
	}
	detail, err := r.Store.Inspect(entry)
	if err != nil || !strings.Contains(detail, "line-01") {
		t.Fatal(detail, err)
	}
	var result string
	if err := r.Store.DB.QueryRow("SELECT result_json FROM tool_calls WHERE provider_call_id='early'").Scan(&result); err != nil || !strings.Contains(result, `"status":"running"`) {
		t.Fatal(result, err)
	}
}
