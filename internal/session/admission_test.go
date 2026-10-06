package session

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"ttc/internal/jobs"
	"ttc/internal/provider"
	"ttc/internal/tool"
)

func admissionTurn(t *testing.T, r *Runtime) string {
	t.Helper()
	turn, err := r.Store.BeginTurn(r.Current(), "async", r.selection)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestFailedAndOversizedAdmissionRetainsNotificationsAndCursor(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe events")
	turn := admissionTurn(t, r)
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal(err)
	}
	r.publishJob(jobs.Snapshot{ID: "job", Owner: "main", Kind: "shell", Status: "completed"})
	if err := r.queueNotification(`{"type":"job_exit","job_id":"job"}`); err != nil {
		t.Fatal(err)
	}
	before := append([]provider.Message(nil), r.notifications...)
	cursor := r.mainContext
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER fail_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err == nil {
		t.Fatal("forced failure succeeded")
	}
	if !reflect.DeepEqual(before, r.notifications) || !reflect.DeepEqual(cursor, r.mainContext) {
		t.Fatal("failed admission consumed events/cursor")
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER fail_admission"); err != nil {
		t.Fatal(err)
	}
	small := r.selection
	small.Model.Budget.ContextLimit = 1
	if _, _, err := r.admitMain(context.Background(), turn, small); !errors.Is(err, errNeedsCompaction) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, r.notifications) || !reflect.DeepEqual(cursor, r.mainContext) {
		t.Fatal("oversized admission consumed events/cursor")
	}
	admitted, _, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || len(admitted.NoticeEntries) != 1 || len(r.notifications) != 0 || r.mainContext.jobs["job"] != "completed" {
		t.Fatal(admitted, err)
	}
}

func TestTimerCoalescingAndExactRequestAcknowledgment(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe timer")
	turn := admissionTurn(t, r)
	v := wakeup{ID: "timer", Name: "check", Message: "Check progress", Status: "scheduled", Repeat: 60}
	r.publishTimer(v)
	v.Fired = 1
	r.publishTimer(v)
	first := r.notifications[0].EventSeq
	v.Fired = 2
	r.publishTimer(v)
	if len(r.notifications) != 1 || r.notifications[0].EventSeq <= first {
		t.Fatal(r.notifications)
	}
	latest := r.notifications[0].EventSeq
	admitted, cm, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	var view runtimeContext
	if err := json.Unmarshal([]byte(cm.Content), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Changes) != 1 || view.Changes[0].FiredCount != 2 || len(view.Timers) != 1 {
		t.Fatal(view)
	}
	var ack string
	if err := r.Store.DB.QueryRow("SELECT delivered_events_json FROM model_requests WHERE id=?", admitted.RequestID).Scan(&ack); err != nil {
		t.Fatal(err)
	}
	var delivered []int64
	if err := json.Unmarshal([]byte(ack), &delivered); err != nil || !reflect.DeepEqual(delivered, []int64{latest}) {
		t.Fatal(ack, err)
	}
	// A firing committed after the cutoff remains pending for the next request.
	v.Fired = 3
	r.publishTimer(v)
	if len(r.notifications) != 1 || r.notifications[0].EventSeq <= admitted.Cutoff {
		t.Fatal(r.notifications, admitted)
	}
	next, cm, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || next.Cutoff <= admitted.Cutoff {
		t.Fatal(next, err)
	}
	if err := json.Unmarshal([]byte(cm.Content), &view); err != nil || view.Changes[0].FiredCount != 3 {
		t.Fatal(view, err)
	}
}

func TestTimerCancellationCannotBeOverwrittenByEarlierFire(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe timer publication")
	entered, release, finished, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	w := newWakeups(context.Background(), func(v wakeup) {
		if v.Status == "scheduled" && v.Fired == 1 {
			close(entered)
			<-release
			r.publishTimer(v)
			close(finished)
			return
		}
		r.publishTimer(v)
		if v.Status == "cancelled" {
			close(cancelled)
		}
	})
	defer w.close()
	delay, repeat := 0, 60
	v, err := w.schedule(wakeupSchedule{Name: "race", Message: "Check", Delay: &delay, Repeat: &repeat}, "main", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timer did not fire")
	}
	stopped := make(chan error, 1)
	go func() { _, err := w.stop(v.ID, ""); stopped <- err }()
	// Correct supervisors serialize stop behind the blocked firing callback.
	// Older code publishes cancellation immediately, then overwrites it on release.
	select {
	case <-cancelled:
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not finish")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fire publication did not finish")
	}
	r.orderMu.Lock()
	state := r.publishedTimers[v.ID]
	r.orderMu.Unlock()
	if state.Status != "cancelled" || state.Fired != 1 {
		t.Fatal("older firing resurrected cancelled timer", state)
	}
}

func TestCommittedNoticeRejectsCopiedSourceAndDuplicatePending(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe committed child finish")
	id, err := r.Store.Append(r.Current(), "", "main/child", "status", "", false, map[string]any{"type": "child_finished"})
	if err != nil {
		t.Fatal(err)
	}
	content := `{"type":"child_turn_finished","child_id":"main/child"}`
	if err := r.queueCommittedNotification(content, id); err != nil {
		t.Fatal(err)
	}
	if err := r.queueCommittedNotification(content, id); err == nil {
		t.Fatal("duplicate source queued")
	}
	if err := r.queueCommittedNotification("invalid JSON", id); err == nil {
		t.Fatal("invalid JSON queued")
	}
	copy, err := r.Store.DB.Exec(`INSERT INTO entries(session_id,parent_id,source_id,turn_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,content_json,file_tip_id,created_ms) SELECT session_id,id,id,turn_id,main_turn_id,undo_owner_turn_id,actor_id,kind,role,model_visible,content_json,file_tip_id,created_ms FROM entries WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	copyID, err := copy.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.queueCommittedNotification(content, copyID); err == nil {
		t.Fatal("copy row accepted as a new semantic source")
	}
	turn := admissionTurn(t, r)
	admitted, _, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || len(admitted.NoticeEntries) != 1 {
		t.Fatal(admitted, err)
	}
}

func TestFailedHumanCheckpointDoesNotLeaveActiveTurnOrUndoOwner(t *testing.T) {
	r, _ := runtimeFixture(t, []provider.ScriptResponse{{Text: "Recovered"}})
	seedRuntime(t, r, "First human instruction")
	var before string
	if err := r.Store.DB.QueryRow("SELECT metadata_json FROM sessions WHERE id=?", r.Current()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER fail_human BEFORE INSERT ON entries WHEN NEW.role='user' BEGIN SELECT RAISE(ABORT,'forced user append failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Rejected instruction"}); err == nil {
		t.Fatal("forced append failure succeeded")
	}
	var after string
	var running int
	if err := r.Store.DB.QueryRow("SELECT metadata_json FROM sessions WHERE id=?", r.Current()).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM turns WHERE session_id=? AND actor_id='main' AND status='running'", r.Current()).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if before != after || running != 0 {
		t.Fatal("rejected user checkpoint changed turn/undo ownership", before, after, running)
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER fail_human"); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "Retry instruction"}); err != nil {
		t.Fatal("main admission did not recover", err)
	}
}

func TestAdmissionUsesPublishedJobStateAtItsCutoff(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe a finishing job")
	turn := admissionTurn(t, r)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	r.Jobs.OnState = func(v jobs.Snapshot) {
		if v.Status != "running" {
			close(entered)
			<-release
		}
		r.publishJob(v)
	}
	id, err := r.Jobs.Start("main", "true", r.Workspace.Root, time.Second, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("terminal publication was not reached")
	}
	// The supervisor already observed exit, but its semantic event is unpublished.
	observed, err := r.Jobs.View("main", id)
	if err != nil || observed.Status == "running" {
		t.Fatal(observed, err)
	}
	admitted, cm, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	var view runtimeContext
	if err := json.Unmarshal([]byte(cm.Content), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 || view.Jobs[0].ID != id || view.Jobs[0].Status != "running" {
		t.Fatal("request observed an uncommitted exit", view)
	}
	var lastStatus string
	if err := r.Store.DB.QueryRow(`SELECT json_extract(content_json,'$.job.status') FROM entries WHERE id<=? AND json_extract(content_json,'$.type')='job_state' AND json_extract(content_json,'$.job.job_id')=? ORDER BY id DESC LIMIT 1`, admitted.Cutoff, id).Scan(&lastStatus); err != nil || lastStatus != "running" {
		t.Fatal(lastStatus, err)
	}
	unblock()
	if _, err := r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	_, cm, err = r.admitMain(context.Background(), turn, r.selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(cm.Content), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 0 || len(view.Changes) != 1 || view.Changes[0].ID != id || view.Changes[0].To != "completed" {
		t.Fatal("committed exit was not delivered at next boundary", view)
	}
}

func TestClosingChildJoinsItsBackgroundCommands(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "explicit_idle_close"
		if failed {
			name = "failed_assignment"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			step := 0
			r.Provider = &childProvider{stream: func(ctx context.Context, request provider.Request, emit func(provider.StreamEvent) error) error {
				step++
				if step == 1 {
					return emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: "background", Name: "shell", Arguments: []byte(`{"command":"sleep 30","background":true,"wake_on_exit":false}`)}})
				}
				if failed {
					return errors.New("synthetic child failure")
				}
				return emit(provider.StreamEvent{Kind: "text", Text: "Background task started"})
			}}
			_, ids := batchIntents(t, r, "main", []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"start a background task","label":"fixture"}`)}})
			record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: ids[0]}, "subagent", []byte(`{"persistent":true,"prompt":"start a background task","label":"fixture"}`))
			var result map[string]any
			if err := json.Unmarshal(record.Result, &result); err != nil {
				t.Fatal(err)
			}
			childID, ok := result["child_id"].(string)
			if !ok {
				t.Fatal(string(record.Result))
			}
			if !failed {
				live := r.Jobs.Live()
				found := false
				for _, job := range live {
					found = found || job.Owner == childID && job.Kind == "shell"
				}
				if !found {
					t.Fatal("fixture did not launch child-owned shell", live)
				}
				if _, err := r.StopChild(context.Background(), "main", childID); err != nil {
					t.Fatal(err)
				}
			}
			for _, job := range r.Jobs.Live() {
				if job.Owner == childID {
					t.Fatal("closed child left an owned process running", job)
				}
			}
			all := r.Jobs.List("main", true)
			found := false
			for _, job := range all {
				if job.Owner == childID && job.Kind == "shell" {
					found = true
					if job.Status != "cancelled" {
						t.Fatal("owned shell not joined before close", job)
					}
				}
			}
			if !found {
				t.Fatal("shell capture was lost", all)
			}
		})
	}
}
