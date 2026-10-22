package session

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func TestUnchangedRuntimeAdmissionsKeepContextBounded(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Inspect the fixture")
	turn := admissionTurn(t, r)
	first, cm, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || cm == nil || first.ContextEntry == 0 {
		t.Fatal(first, cm, err)
	}
	input := r.UsageSnapshot().Input
	for range 99 {
		admitted, cm, err := r.admitMain(context.Background(), turn, r.selection)
		if err != nil || cm != nil || admitted.ContextEntry != 0 {
			t.Fatal(admitted, cm, err)
		}
		if !reflect.DeepEqual(admitted.Messages, first.Messages) || r.UsageSnapshot().Input != input {
			t.Fatal("unchanged boundaries grew model input")
		}
	}
	var requests, distinctInput, snapshots, metadataBytes int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*),count(DISTINCT input_json),max(length(input_json)) FROM model_requests").Scan(&requests, &distinctInput, &metadataBytes); err != nil {
		t.Fatal(err)
	}
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM entries WHERE role='developer'").Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if requests != 100 || distinctInput != 1 || snapshots != 1 || metadataBytes > 384 {
		t.Fatal("snapshot overhead accumulated", requests, distinctInput, snapshots, metadataBytes)
	}
}

func TestAdmissionWithoutSnapshotDeliversNoticeAndSteering(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe messages")
	turn := admissionTurn(t, r)
	if _, _, err := r.admitMain(context.Background(), turn, r.selection); err != nil {
		t.Fatal(err)
	}
	if err := r.queueNotification(`{"type":"image_click_cancelled"}`); err != nil {
		t.Fatal(err)
	}
	source := r.notifications[0].EventSeq
	r.steers = []contextbuild.Input{{Text: "Also inspect units."}}
	cursor := r.mainContext
	if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER fail_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, cm, err := r.admitMain(context.Background(), turn, r.selection); err == nil || cm != nil {
		t.Fatal("failed admission unexpectedly succeeded or created snapshot", cm, err)
	}
	if !reflect.DeepEqual(r.mainContext, cursor) || len(r.notifications) != 1 || len(r.steers) != 1 {
		t.Fatal("failed admission consumed input")
	}
	if _, err := historytest.DB(t, r.Store).Exec("DROP TRIGGER fail_admission"); err != nil {
		t.Fatal(err)
	}
	admitted, cm, err := r.admitMain(context.Background(), turn, r.selection)
	if err != nil || cm != nil || admitted.ContextEntry != 0 || len(admitted.NoticeEntries) != 1 || len(admitted.SteerEntries) != 1 {
		t.Fatal(admitted, cm, err)
	}
	if len(r.notifications) != 0 || len(r.steers) != 0 || admitted.Cutoff < source {
		t.Fatal("successful admission did not consume eligible input")
	}
	last := admitted.Messages[len(admitted.Messages)-1]
	if last.Content != "Also inspect units." || last.Type.IsRuntime() {
		t.Fatal("steering not delivered", last)
	}
	var delivered int64
	if err := historytest.DB(t, r.Store).QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", source).Scan(&delivered); err != nil || delivered != admitted.RequestID {
		t.Fatal("notice not acknowledged", delivered, err)
	}
}

func TestRuntimeSnapshotTracksMetadataAndChildState(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe runtime state")
	selection := r.selection
	_, cursor, err := r.runtimeContext(context.Background(), "main", selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	changes := []struct {
		name  string
		apply func()
	}{
		{"model", func() { selection.Model.ID = "other-model" }},
		{"variant", func() { selection.Variant = "fast" }},
		{"image input", func() { selection.Model.Images = !selection.Model.Images }},
		{"binary formats", func() {
			selection.Model.BinaryFiles = []llm.BinaryFileType{{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: 32 << 20}}
		}},
		{"image click", func() {
			r.images.mu.Lock()
			r.images.enabled = !r.images.enabled
			r.images.mu.Unlock()
		}},
		{"date", func() {
			old := *cursor.snapshot
			old.Date = "1900-01-01"
			cursor.snapshot = &old
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			change.apply()
			m, next, err := r.runtimeContext(context.Background(), "main", selection, cursor)
			if err != nil || m == nil {
				t.Fatal("metadata change omitted", m, err)
			}
			cursor = next
			m, _, err = r.runtimeContext(context.Background(), "main", selection, cursor)
			if err != nil || m != nil {
				t.Fatal("metadata repeated", m, err)
			}
		})
	}
	for _, state := range []string{"running", "idle", "closed"} {
		if err := r.publishChild(tool.ChildView{ID: "main/child_fixture", Label: "Fixture", ChildState: state, AssignmentStatus: "running"}); err != nil {
			t.Fatal(err)
		}
		m, next, err := r.runtimeContext(context.Background(), "main", selection, cursor)
		if err != nil || m != nil {
			t.Fatal("child activity emitted context", state, m, err)
		}
		fresh, _, err := r.runtimeContext(context.Background(), "main", selection, contextCursor{})
		if err != nil {
			t.Fatal(err)
		}
		var view runtimeContext
		if err := json.Unmarshal([]byte(fresh.Content), &view); err != nil {
			t.Fatal(err)
		}
		if state == "closed" {
			if len(view.Children) != 0 {
				t.Fatal("closed child retained", view.Children)
			}
		} else if len(view.Children) != 1 || view.Children[0].ChildState != state {
			t.Fatal("fresh baseline has wrong child state", view.Children)
		}

		cursor = next
		if m, _, err := r.runtimeContext(context.Background(), "main", selection, cursor); err != nil || m != nil {
			t.Fatal("child transition repeated", state, m, err)
		}
	}
}

func TestRuntimeJobAndTimerActivityDoesNotEmitContext(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe transitions")
	_, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "completed"} {
		r.publishJob(jobs.Snapshot{ID: "job", Owner: "main", Status: status})
		m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
		if err != nil || m != nil {
			t.Fatal("job activity emitted context", m, err)
		}
		cursor = next
	}
	for fired := 0; fired <= 2; fired++ {
		r.publishTimer(wakeup{ID: "timer", actor: "main", Name: "Observe progress", Status: "scheduled", Repeat: 60, Fired: fired})
		m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
		if err != nil || m != nil {
			t.Fatal("timer activity emitted context", m, err)
		}
		cursor = next
	}
}

func TestChildAdmissionsOwnIndependentSnapshotCursors(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe children")
	for _, actor := range []string{"main/child_a", "main/child_b"} {
		turn, err := r.Store.BeginChildTurn(r.Current(), actor, r.selection)
		if err != nil {
			t.Fatal(err)
		}
		task := childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools, estimator: &contextbuild.Estimator{}}
		messages := []llm.Message{{Type: llm.TypeUserInput, Content: "Inspect units"}}
		first, cursor, err := r.admitChild(context.Background(), task, messages, contextCursor{})
		if err != nil || first.ContextEntry == 0 {
			t.Fatal(first, err)
		}
		second, next, err := r.admitChild(context.Background(), task, first.Messages, cursor)
		if err != nil || second.ContextEntry != 0 || !reflect.DeepEqual(first.Messages, second.Messages) || !reflect.DeepEqual(cursor, next) {
			t.Fatal("unchanged child context grew", second, err)
		}
	}
	if r.mainContext.snapshot != nil {
		t.Fatal("child admission changed parent cursor")
	}
}

func TestSessionReloadForcesFreshRuntimeSnapshot(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
	}}
	for _, prompt := range []string{"Initial request", "Same runtime state"} {
		if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Command("/load " + r.Current()); err != nil {
		t.Fatal(err)
	}
	if r.mainContext.snapshot != nil {
		t.Fatal("reload retained runtime cursor")
	}
	if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "After reload"}); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := historytest.DB(t, r.Store).QueryRow("SELECT count(*) FROM entries WHERE role='developer' AND source_id IS NULL").Scan(&snapshots); err != nil || snapshots != 2 {
		t.Fatal("reload did not force fresh context", snapshots, err)
	}
	messages, err := r.Store.Messages(r.Current())
	if err != nil {
		t.Fatal(err)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role() == "developer" && messages[i].Type.IsRuntime() {
			if nativeAnnouncement(t, &messages[i]) == nil {
				t.Fatal("reload omitted native-read capabilities")
			}
			return
		}
	}
	t.Fatal("reload omitted runtime context")
}
