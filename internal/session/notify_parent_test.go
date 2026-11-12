package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ttc/internal/historytest"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func parentNotifyFixture(t *testing.T) (*Runtime, *codingChild) {
	t.Helper()
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Notification test")
	child := &codingChild{id: "main/child_notify", state: "running", turn: "ct_notify", job: "job_notify", tools: tool.NewRegistry(), assignment: &childAssignment{accepting: true}}
	r.addNotifyParentTool(child.tools, child.id)
	r.coordinator.children[child.id] = child
	r.actorAssignments[child.id] = child.assignment
	return r, child
}

func invokeParentNotify(ctx context.Context, child *codingChild, actor, message string) map[string]any {
	raw, _ := json.Marshal(map[string]string{"message": message})
	record := child.tools.Invoke(ctx, tool.Execution{Actor: actor}, "notify_parent", raw)
	var result map[string]any
	if err := json.Unmarshal(record.Result, &result); err != nil {
		panic(err)
	}
	return result
}

func notificationError(result map[string]any) string {
	if failure, ok := result["error"].(map[string]any); ok {
		code, _ := failure["code"].(string)
		return code
	}
	return ""
}

func TestNotifyParentPermissionsValidationAndAssignmentGate(t *testing.T) {
	r, child := parentNotifyFixture(t)
	if _, ok := r.Tools.Get("notify_parent"); ok {
		t.Fatal("main advertises child-only tool")
	}
	for _, actor := range []string{"main", "main/aside", "main/sibling"} {
		if got := notificationError(invokeParentNotify(context.Background(), child, actor, "blocker")); got != "ownership" {
			t.Fatal(actor, got)
		}
	}
	for _, text := range []string{"", " \n\t", strings.Repeat("é", 1025)} {
		if got := notificationError(invokeParentNotify(context.Background(), child, child.id, text)); got != "invalid_input" {
			t.Fatal(got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := invokeParentNotify(ctx, child, child.id, "cancelled"); got["ok"] == true {
		t.Fatal(got)
	}
	for _, state := range []string{"idle", "closed"} {
		child.state = state
		if got := notificationError(invokeParentNotify(context.Background(), child, child.id, "late")); got != "assignment_inactive" {
			t.Fatal(state, got)
		}
	}
	child.state = "running"
	child.closing = true
	if got := notificationError(invokeParentNotify(context.Background(), child, child.id, "closing")); got != "assignment_inactive" {
		t.Fatal(got)
	}
	child.closing = false
	child.assignment.accepting = false
	if got := notificationError(invokeParentNotify(context.Background(), child, child.id, "settled")); got != "assignment_inactive" {
		t.Fatal(got)
	}
	if child.assignment.parentNotifications != 0 || len(pendingMainNotices(t, r)) != 0 {
		t.Fatal("rejected send persisted input")
	}
}

func TestNotifyParentDurableIdentityAndAssignmentLimits(t *testing.T) {
	r, child := parentNotifyFixture(t)
	message := strings.Repeat("é", 1024)
	for range maxParentNotificationsPerAssignment {
		got := invokeParentNotify(context.Background(), child, child.id, message)
		if got["ok"] != true || got["state"] != "queued" || got["delivered"] != false {
			t.Fatal(got)
		}
	}
	if got := notificationError(invokeParentNotify(context.Background(), child, child.id, "one too many")); got != "notification_capacity" {
		t.Fatal(got)
	}
	notices := pendingMainNotices(t, r)
	if len(notices) == 0 {
		t.Fatal("no pending durable input")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(notices[0].Content), &body); err != nil {
		t.Fatal(err)
	}
	if body["type"] != "child_notification" || body["child_id"] != child.id || body["child_turn_id"] != child.turn || body["job_id"] != child.job || body["message"] != message {
		t.Fatal(body)
	}
	var count int
	if err := historytest.DB(t, r.Store).QueryRow(`SELECT count(*) FROM entries WHERE kind='status' AND actor_id=? AND json_extract(content_json,'$.body.type')='child_notification'`, "main").Scan(&count); err != nil || count != maxParentNotificationsPerAssignment {
		t.Fatal(count, err)
	}
	// Admission/delivery cannot replenish the lifetime assignment allowance.
	r.notifications = runtimeNoticeBacklog{}
	if got := notificationError(invokeParentNotify(context.Background(), child, child.id, "after delivery")); got != "notification_capacity" {
		t.Fatal(got)
	}
	child.assignment = &childAssignment{accepting: true}
	r.actorAssignments[child.id] = child.assignment
	child.turn = "ct_followup"
	child.job = "job_followup"
	if got := invokeParentNotify(context.Background(), child, child.id, "new assignment"); got["ok"] != true {
		t.Fatal(got)
	}
	if !child.assignment.accepting || child.state != "running" {
		t.Fatal("send finished assignment")
	}
}

func TestNotifyParentWakesAwaitAndAdmitsMainOnly(t *testing.T) {
	r, child := parentNotifyFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.awaitActor(context.Background(), "main", time.Now().Add(5*time.Second))
		done <- err
	}()
	if got := invokeParentNotify(context.Background(), child, child.id, "Blocked on required dataset choice"); got["ok"] != true {
		t.Fatal(got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification did not wake main")
	}
	if len(pendingChildNotices(t, r, child.assignment)) != 0 {
		t.Fatal("notification routed back to child")
	}
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		found := 0
		for _, m := range req.Messages {
			if strings.Contains(m.Content, `"type":"child_notification"`) {
				found++
			}
		}
		if found != 1 {
			t.Errorf("main notification count %d", found)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Decision received"})
	}}
	if _, err := r.Run(nil); err != nil {
		t.Fatal(err)
	}
	if r.HasNotifications() {
		t.Fatal("notification not acknowledged")
	}
	if child.state != "running" || !child.assignment.accepting {
		t.Fatal("notification finished assignment")
	}
}

func TestNotifyParentRecoveryPreservesUndeliveredBody(t *testing.T) {
	r, child := parentNotifyFixture(t)
	got := invokeParentNotify(context.Background(), child, child.id, "Need an explicit dataset decision")
	if got["ok"] != true {
		t.Fatal(got)
	}
	turn, err := r.Store.BeginTurn(r.Current(), "user", r.CurrentSelection())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.StartRequest(r.Current(), turn, "main", "compaction", r.CurrentSelection()); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Store.Load(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	notices, err := r.Store.PendingRecoveryNotifications(context.Background(), loaded.ID)
	if err != nil || len(notices) != 1 {
		t.Fatal(notices, err)
	}
	if notices[0].RecoveredFrom != int64(got["message_event_seq"].(float64)) || !strings.Contains(string(notices[0].Body), `"type":"child_notification"`) || !strings.Contains(string(notices[0].Body), "Need an explicit dataset decision") {
		t.Fatal(notices)
	}
}

func TestNotifyParentBusyMainDefersToSettledRequest(t *testing.T) {
	r, child := parentNotifyFixture(t)
	r.Emit = nil
	started, release := make(chan struct{}), make(chan struct{})
	requests := 0
	r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		requests++
		if requests == 1 {
			close(started)
			<-release
		}
		found := 0
		for _, message := range req.Messages {
			if strings.Contains(message.Content, `"type":"child_notification"`) {
				found++
			}
		}
		if requests == 1 && found != 0 || requests == 2 && found != 1 {
			t.Errorf("request %d has %d intermediate notices", requests, found)
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Continue work"})
	}}
	done := make(chan error, 1)
	go func() { _, err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "work"}); done <- err }()
	receive(t, started)
	if got := invokeParentNotify(context.Background(), child, child.id, "Milestone ready for review"); got["ok"] != true {
		t.Fatal(got)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("main failed to settle")
	}
	if requests != 2 {
		t.Fatal("queued notification did not continue main", requests)
	}
}

func TestNotifyParentDoesNotReleaseForegroundChildWait(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	notified, release := make(chan struct{}), make(chan struct{})
	r.Provider = &childProvider{stream: func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
		r.coordinator.childMu.Lock()
		child := r.coordinator.children[req.ConversationID]
		r.coordinator.childMu.Unlock()
		if child == nil {
			t.Error("missing active child")
			return nil
		}
		got := invokeParentNotify(ctx, child, child.id, "Milestone: independent checks passed")
		if got["ok"] != true {
			t.Error(got)
		}
		close(notified)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "Final handoff"})
	}}
	args := `{"persistent":true,"prompt":"task","label":"notification checks","background":false}`
	_, ids := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	done := make(chan map[string]any, 1)
	go func() { done <- childInvocation(t, r, ids[0], args) }()
	receive(t, notified)
	select {
	case result := <-done:
		t.Fatal("intermediate send released foreground wait", result)
	default:
	}
	if !r.HasNotifications() {
		t.Error("foreground notification was not queued")
	}
	close(release)
	select {
	case result := <-done:
		if result["assignment_status"] != "completed" {
			t.Fatal(result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground child did not finish")
	}
}
