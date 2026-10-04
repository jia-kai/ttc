package jobs

import (
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestLiveStableStartOrderAndOtherListsKeepIDOrder(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, fixture := range []struct {
		id     string
		delta  time.Duration
		status string
	}{{"z", 0, "running"}, {"c", 100 * time.Millisecond, "running"}, {"b", 100 * time.Millisecond, "running"}, {"a", 200 * time.Millisecond, "running"}, {"f", -time.Second, "completed"}} {
		at := base.Add(fixture.delta)
		done := make(chan struct{})
		close(done)
		m.jobs[fixture.id] = &job{
			startedAt: at,
			view:      Snapshot{ID: fixture.id, Owner: "main", Status: fixture.status, StartedAt: at.Format(time.RFC3339Nano)},
			stdout:    m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2),
			cancel: func() {}, done: done,
		}
	}
	ids := func(snapshots []Snapshot) []string {
		out := make([]string, len(snapshots))
		for i, v := range snapshots {
			out[i] = v.ID
		}
		return out
	}
	for refresh := range 100 {
		m.jobs["z"].stdout.Write([]byte("output"))
		m.jobs["c"].view.Label = "refreshed label"
		if got := ids(m.Live()); !reflect.DeepEqual(got, []string{"z", "b", "c", "a"}) {
			t.Fatalf("refresh %d reordered jobs: %v", refresh, got)
		}
	}
	if got := ids(m.List("main", false)); !reflect.DeepEqual(got, []string{"a", "b", "c", "z"}) {
		t.Fatal("List no longer uses ID order", got)
	}
	if got := ids(m.Metadata("main")); !reflect.DeepEqual(got, []string{"a", "b", "c", "f", "z"}) {
		t.Fatal("Metadata no longer uses ID order", got)
	}
}

func TestLaunchTimestampRetainsPrecision(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	worker := func(ctx context.Context, _, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	before := time.Now().UTC()
	task, err := m.StartTask("main", "subagent", "task", true, false, worker)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := m.Start("main", "sleep 10", t.TempDir(), 0, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range []string{task, shell} {
		j := m.jobs[id]
		at, err := time.Parse(time.RFC3339Nano, j.view.StartedAt)
		if err != nil || !at.Equal(j.startedAt) || at.Before(before) || at.After(after) {
			t.Fatalf("launch timestamp lost precision: %q, %v", j.view.StartedAt, err)
		}
	}
}
