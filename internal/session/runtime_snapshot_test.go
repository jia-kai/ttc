package session

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	contextbuild "scicode/internal/context"
	"scicode/internal/jobs"
	"scicode/internal/provider"
	"scicode/internal/tool"
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
	if err := r.Store.DB.QueryRow("SELECT count(*),count(DISTINCT input_json),max(length(input_json)) FROM model_requests").Scan(&requests, &distinctInput, &metadataBytes); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE role='developer'").Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if requests != 100 || distinctInput != 1 || snapshots != 1 || metadataBytes > 128 {
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
	if _, err := r.Store.DB.Exec(`CREATE TRIGGER fail_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, cm, err := r.admitMain(context.Background(), turn, r.selection); err == nil || cm != nil {
		t.Fatal("failed admission unexpectedly succeeded or created snapshot", cm, err)
	}
	if !reflect.DeepEqual(r.mainContext, cursor) || len(r.notifications) != 1 || len(r.steers) != 1 {
		t.Fatal("failed admission consumed input")
	}
	if _, err := r.Store.DB.Exec("DROP TRIGGER fail_admission"); err != nil {
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
	if last.Content != "Also inspect units." || last.Runtime {
		t.Fatal("steering not delivered", last)
	}
	var delivered int64
	if err := r.Store.DB.QueryRow("SELECT delivered_request_id FROM entries WHERE id=?", source).Scan(&delivered); err != nil || delivered != admitted.RequestID {
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
		{"image click", func() {
			r.images.mu.Lock()
			r.images.enabled = !r.images.enabled
			r.images.mu.Unlock()
		}},
		{"date", func() {
			var old map[string]any
			if err := json.Unmarshal([]byte(cursor.snapshot), &old); err != nil {
				t.Fatal(err)
			}
			old["date_utc"] = "1900-01-01"
			data, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			cursor.snapshot = string(data)
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
		if err := r.publishChild(tool.ChildView{ID: "main/child_fixture", Label: "Fixture", State: state}); err != nil {
			t.Fatal(err)
		}
		m, next, err := r.runtimeContext(context.Background(), "main", selection, cursor)
		if err != nil || m == nil {
			t.Fatal("child transition omitted", state, err)
		}
		var view runtimeContext
		if err := json.Unmarshal([]byte(m.Content), &view); err != nil {
			t.Fatal(err)
		}
		if state == "closed" {
			if len(view.Children) != 0 {
				t.Fatal("closed child retained", view.Children)
			}
		} else if len(view.Children) != 1 || view.Children[0].State != state {
			t.Fatal("wrong child state", view.Children)
		}
		cursor = next
		if m, _, err := r.runtimeContext(context.Background(), "main", selection, cursor); err != nil || m != nil {
			t.Fatal("child transition repeated", state, m, err)
		}
	}
}

func TestRuntimeJobAndTimerTransitionsAreSuppliedOnce(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe transitions")
	_, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil {
		t.Fatal(err)
	}
	exit := 1
	for _, job := range []jobs.Snapshot{
		{ID: "job", Owner: "main", Status: "running"},
		{ID: "job", Owner: "main", Status: "completed", ExitCode: &exit},
	} {
		r.publishJob(job)
		m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
		if err != nil || m == nil {
			t.Fatal("job transition omitted", job, err)
		}
		var view runtimeContext
		if err := json.Unmarshal([]byte(m.Content), &view); err != nil || len(view.Changes) != 1 {
			t.Fatal(view, err)
		}
		if job.Status == "completed" && (view.Changes[0].To != "failed" || len(view.Jobs) != 0) {
			t.Fatal("failed job lost", view)
		}
		cursor = next
		if m, _, err := r.runtimeContext(context.Background(), "main", r.selection, cursor); err != nil || m != nil {
			t.Fatal("job transition repeated", m, err)
		}
	}
	for fired := 0; fired <= 2; fired++ {
		r.publishTimer(wakeup{ID: "timer", Name: "Observe progress", Status: "scheduled", Repeat: 60, Fired: fired})
		m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
		if err != nil || m == nil {
			t.Fatal("timer firing omitted", fired, err)
		}
		var view runtimeContext
		if err := json.Unmarshal([]byte(m.Content), &view); err != nil || len(view.Changes) != 1 || view.Changes[0].FiredCount != fired {
			t.Fatal(view, err)
		}
		cursor = next
		if m, _, err := r.runtimeContext(context.Background(), "main", r.selection, cursor); err != nil || m != nil {
			t.Fatal("timer transition repeated", fired, m, err)
		}
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
		task := childTask{actor: actor, turn: turn, selection: r.selection, tools: r.Tools}
		messages := []provider.Message{{Role: "user", Content: "Inspect units"}}
		first, cursor, err := r.admitChild(context.Background(), task, messages, contextCursor{})
		if err != nil || first.ContextEntry == 0 {
			t.Fatal(first, err)
		}
		second, next, err := r.admitChild(context.Background(), task, first.Messages, cursor)
		if err != nil || second.ContextEntry != 0 || !reflect.DeepEqual(first.Messages, second.Messages) || !reflect.DeepEqual(cursor, next) {
			t.Fatal("unchanged child context grew", second, err)
		}
	}
	if r.mainContext.snapshot != "" {
		t.Fatal("child admission changed parent cursor")
	}
}

func TestSessionReloadForcesFreshRuntimeSnapshot(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &childProvider{stream: func(_ context.Context, _ provider.Request, emit func(provider.StreamEvent) error) error {
		return emit(provider.StreamEvent{Kind: "text", Text: "Done"})
	}}
	for _, prompt := range []string{"Initial request", "Same runtime state"} {
		if err := r.Run(&provider.Message{Role: "user", Content: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Command("/load " + r.Current()); err != nil {
		t.Fatal(err)
	}
	if r.mainContext.snapshot != "" || r.mainContext.project != "" {
		t.Fatal("reload retained runtime cursor")
	}
	if err := r.Run(&provider.Message{Role: "user", Content: "After reload"}); err != nil {
		t.Fatal(err)
	}
	var snapshots int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE role='developer' AND source_id IS NULL").Scan(&snapshots); err != nil || snapshots != 2 {
		t.Fatal("reload did not force fresh context", snapshots, err)
	}
}
