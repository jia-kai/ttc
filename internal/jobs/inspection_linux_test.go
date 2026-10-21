package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/capture"
)

func TestInspectOwnershipContextAndCallbackOutsideLocks(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	var calls atomic.Int32
	inspector := func(view Snapshot) InspectionSpec {
		calls.Add(1)
		if _, err := m.View("main", view.ID); err != nil {
			t.Error(err)
		}
		return InspectionSpec{Title: "custom", Fields: []InspectionField{{Name: "Identity", Value: view.Label}}}
	}
	var launchInspected atomic.Bool
	m.OnState = func(view Snapshot) {
		if view.Status == "running" {
			inspection, err := m.Inspect(context.Background(), "main", view.ID)
			if err != nil || inspection.Spec.Title != "custom" {
				t.Errorf("inspector not installed before publication: %+v, %v", inspection, err)
			}
			launchInspected.Store(true)
		}
	}
	id, err := m.StartTask("main/child/nested", TaskOptions{Kind: "fixture", Label: "task", Inspector: inspector}, func(context.Context, io.Writer, io.Writer) (TaskResult, error) {
		if !launchInspected.Load() {
			t.Error("worker preceded inspection publication")
		}
		return TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for _, owner := range []string{"main/other", "main/child/nested/descendant", "other"} {
		if _, err := m.Inspect(context.Background(), owner, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("owner %q: %v", owner, err)
		}
	}
	if _, err := m.Inspect(context.Background(), "main", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Inspect(ctx, "main", id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("inaccessible or cancelled inspection invoked callback")
	}
	for _, owner := range []string{"main", "main/child", "main/child/nested"} {
		if _, err := m.Inspect(context.Background(), owner, id); err != nil {
			t.Fatalf("owner %q: %v", owner, err)
		}
	}
}

func TestInspectImmutableSnapshotSpecAndPostCompletionRetention(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	fields := []InspectionField{{Name: "Identity", Value: "original"}}
	streams := []InspectionStream{{Name: "stdout", Heading: "output", LimitBytes: 8192}}
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Label: "original", Inspector: func(view Snapshot) InspectionSpec {
		if view.ExitCode != nil {
			*view.ExitCode = 99
		}
		view.Label = "changed"
		return InspectionSpec{Fields: fields, Streams: streams}
	}}, func(_ context.Context, out, _ io.Writer) (TaskResult, error) {
		io.WriteString(out, "retained\n\n")
		code := 7
		return TaskResult{ExitCode: &code}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	if err != nil || inspection.Job.Label != "original" || inspection.Job.ExitCode == nil || *inspection.Job.ExitCode != 7 || inspection.Job.Status != "completed" || inspection.Output[0].Text != "retained\n\n" || inspection.Output[0].Truncated {
		t.Fatalf("inspection: %+v, %v", inspection, err)
	}
	fields[0].Value = "mutated"
	streams[0].Heading = "mutated"
	if inspection.Spec.Fields[0].Value != "original" || inspection.Spec.Streams[0].Heading != "output" || inspection.Output[0].Heading != "output" {
		t.Fatal("spec retained callback-owned slices")
	}
	*inspection.Job.ExitCode = 42
	view, err := m.View("main", id)
	if err != nil || *view.ExitCode != 7 {
		t.Fatal("inspection mutated manager metadata", view, err)
	}
	*view.ExitCode = 43
	if again, err := m.View("main", id); err != nil || *again.ExitCode != 7 {
		t.Fatal("view mutated manager metadata", again, err)
	}
	data, err := json.Marshal(inspection.Job)
	if err != nil || strings.Contains(string(data), "Inspector") {
		t.Fatal("callback leaked into snapshot", string(data), err)
	}
	m.Close()
	if _, err := m.Inspect(context.Background(), "main", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("closed handle revived", err)
	}
}

func TestInspectDefaultTaskSpecAndIndependentStreamBudgets(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Label: "default task"}, func(_ context.Context, out, stderr io.Writer) (TaskResult, error) {
		io.WriteString(out, strings.Repeat("a", 9000))
		io.WriteString(stderr, strings.Repeat("b", 9000))
		return TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	wantStreams := []InspectionStream{{Name: "stdout", Heading: "stdout · showing tail, up to 8 KiB", LimitBytes: 8192}, {Name: "stderr", Heading: "stderr · showing tail, up to 8 KiB", LimitBytes: 8192}}
	if err != nil || !reflect.DeepEqual(inspection.Spec.Fields, []InspectionField{{Name: "Label", Value: "default task"}}) || !reflect.DeepEqual(inspection.Spec.Streams, wantStreams) {
		t.Fatalf("default task spec: %+v, %v", inspection, err)
	}
	for i, output := range inspection.Output {
		if len(output.Text) != 8192 || output.Text != strings.Repeat(string("ab"[i]), 8192) || !output.Truncated || output.Error != "" {
			t.Fatalf("stream budget %d: len=%d, truncated=%v, error=%q", i, len(output.Text), output.Truncated, output.Error)
		}
	}
}

func TestInspectKeepsCallbackMetadataWhenJobCompletes(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Inspector: func(view Snapshot) InspectionSpec {
		if _, err := m.Stop("main", view.ID); err != nil {
			t.Error(err)
		}
		return InspectionSpec{Title: view.Status, Streams: defaultInspectionStreams(false)}
	}}, func(ctx context.Context, out, _ io.Writer) (TaskResult, error) {
		<-ctx.Done()
		io.WriteString(out, "terminal output")
		return TaskResult{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	if err != nil || inspection.Job.Status != "running" || inspection.Job.FinishedAt != "" || inspection.Spec.Title != inspection.Job.Status || inspection.Output[0].Text != "terminal output" {
		t.Fatalf("metadata refreshed after callback: %+v, %v", inspection, err)
	}
}

func TestInspectValidatesBoundsAndPreservesBoundedTails(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	requested := []InspectionStream{
		{Name: "stdout", LimitBytes: 65536},
		{Name: "stdout", LimitBytes: 5},
		{Name: "stdout", LimitBytes: 0},
		{Name: "stdout", LimitBytes: -1},
		{Name: "stdout", LimitBytes: 65537},
		{Name: "other", LimitBytes: 8192},
		{Name: "stderr", LimitBytes: 8192},
	}
	text := strings.Repeat("x", 70000) + "last\n"
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Inspector: func(Snapshot) InspectionSpec {
		return InspectionSpec{Streams: requested}
	}}, func(_ context.Context, out, stderr io.Writer) (TaskResult, error) {
		io.WriteString(out, text)
		io.WriteString(stderr, "diagnostic")
		return TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	inspection, err := m.Inspect(context.Background(), "main", id)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{text[len(text)-65536:], "last\n"} {
		output := inspection.Output[i]
		if output.Error != "" || output.Text != want || !output.Truncated || len(output.Text) > output.LimitBytes {
			t.Fatalf("tail %d: len=%d, truncated=%v, error=%q", i, len(output.Text), output.Truncated, output.Error)
		}
	}
	for _, i := range []int{2, 3, 4, 5} {
		if inspection.Output[i].Error == "" || inspection.Output[i].Text != "" {
			t.Fatalf("invalid request %d acquired output: %+v", i, inspection.Output[i])
		}
	}
	if inspection.Output[6].Text != "diagnostic" || inspection.Output[6].Truncated {
		t.Fatal(inspection.Output[6])
	}
}

func TestInspectAllTailsBoundedDuringWrites(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	ready := make(chan struct{})
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Inspector: func(Snapshot) InspectionSpec {
		return InspectionSpec{Streams: []InspectionStream{{Name: "stdout", LimitBytes: 65536}, {Name: "stderr", LimitBytes: 65536}, {Name: "stdout", LimitBytes: 65536}, {Name: "stderr", LimitBytes: 65536}}}
	}}, func(ctx context.Context, out, stderr io.Writer) (TaskResult, error) {
		close(ready)
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return TaskResult{}, ctx.Err()
			default:
			}
			fmt.Fprintf(out, "%d\n", i)
			fmt.Fprintf(stderr, "%d\n", i)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	for range 100 {
		inspection, err := m.Inspect(context.Background(), "main", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, output := range inspection.Output {
			if output.Error != "" || len(output.Text) > output.LimitBytes {
				t.Fatal("capture exceeded acquisition bound", output)
			}
		}
	}
}

func TestInspectCommandAndLSPDefaultsRejectProtocolStdout(t *testing.T) {
	for _, protocol := range []bool{false, true} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			m := New(context.Background(), nil)
			defer m.Close()
			command := "printf diagnostic >&2; sleep 10"
			var launch Inspection
			m.OnState = func(view Snapshot) {
				if view.Status == "running" {
					var err error
					launch, err = m.Inspect(context.Background(), "main", view.ID)
					if err != nil {
						t.Error(err)
					}
				}
			}
			var id string
			var err error
			if protocol {
				id, err = m.StartLSP("main", command, t.TempDir(), 0, true, false, capture.OutputOptions{}, "")
			} else {
				id, err = m.Start("main", command, t.TempDir(), 0, true, true, false, capture.OutputOptions{}, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(launch.Spec.Fields, []InspectionField{{Name: "Command", Value: command, Language: "sh"}}) || !reflect.DeepEqual(launch.Spec.Streams, defaultInspectionStreams(protocol)) {
				t.Fatal("command inspector missing at launch", launch.Spec)
			}
			if !protocol {
				return
			}
			m.mu.Lock()
			m.jobs[id].inspector = func(Snapshot) InspectionSpec {
				return InspectionSpec{Streams: []InspectionStream{{Name: "stdout", LimitBytes: 8192}, {Name: "stderr", LimitBytes: 8192}}}
			}
			m.mu.Unlock()
			inspection, err := m.Inspect(context.Background(), "main", id)
			if err != nil || len(inspection.Output) != 2 || inspection.Output[0].Error == "" || inspection.Output[0].Text != "" || inspection.Output[1].Error != "" {
				t.Fatalf("custom LSP stdout not rejected: %+v, %v", inspection, err)
			}
			if _, err := m.Stop("main", id); err != nil {
				t.Fatal(err)
			}
			inspection, err = m.Inspect(context.Background(), "main", id)
			if err != nil || inspection.Output[0].Error == "" {
				t.Fatal("completed LSP stdout not rejected", inspection, err)
			}
		})
	}
}

func TestInspectChecksContextAfterCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(context.Background(), nil)
	defer m.Close()
	id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Inspector: func(Snapshot) InspectionSpec {
		cancel()
		return taskInspector(Snapshot{})
	}}, func(context.Context, io.Writer, io.Writer) (TaskResult, error) { return TaskResult{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Inspect(ctx, "main", id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInspectWatchIdentityIsCapturedBeforeLaunch(t *testing.T) {
	dir, _ := watchFixture(t, true)
	m := New(context.Background(), nil)
	defer m.Close()
	var launch Inspection
	m.OnState = func(view Snapshot) {
		if view.Status == "running" {
			var err error
			launch, err = m.Inspect(context.Background(), "main", view.ID)
			if err != nil {
				t.Error(err)
			}
		}
	}
	id, err := m.StartWatch("main", dir, false, "", capture.OutputOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := m.Wait(ctx, "main", id, nil); err != nil {
		t.Fatal(err)
	}
	want := []InspectionField{{Name: "Record dir", Value: dir}, {Name: "Command", Value: "test-workload", Language: "sh"}}
	if launch.Job.RecordDir != dir || !reflect.DeepEqual(launch.Spec.Fields, want) {
		t.Fatal("watch identity not ready at launch", launch)
	}
	data, err := json.Marshal(launch.Job)
	if err != nil || !strings.Contains(string(data), `"record_dir":`) {
		t.Fatal("watch JSON contract lost record_dir", string(data), err)
	}
	// Presentation is based on the captured identity, not mutable snapshots.
	m.mu.Lock()
	m.jobs[id].view.RecordDir = "changed"
	m.jobs[id].view.Label = "changed"
	m.mu.Unlock()
	inspection, err := m.Inspect(ctx, "main", id)
	if err != nil || !reflect.DeepEqual(inspection.Spec.Fields, want) || inspection.Output[0].Text != "complete output\n" {
		t.Fatalf("watch inspector refreshed immutable identity: %+v, %v", inspection, err)
	}
}
