package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/watchrun"
)

func TestRuntimeJobTransitionsPreserveCanonicalStatus(t *testing.T) {
	zero, seven, signaled := 0, 7, 143
	for _, kind := range []string{"shell", "watch"} {
		for _, tc := range []struct {
			name   string
			status string
			code   *int
			signal string
		}{
			{"zero", "completed", &zero, ""},
			{"nonzero", "completed", &seven, ""},
			{"signal", "completed", nil, "terminated"},
			{"signal_with_code", "completed", &signaled, "terminated"},
			{"cancelled", "cancelled", &seven, "terminated"},
			{"supervision_failure", "failed", &zero, ""},
			{"supervision_failure_nonzero", "failed", &seven, ""},
			{"unknown_exit", "interrupted", nil, ""},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				seedRuntime(t, r, "Observe canonical job state")
				r.publishJob(jobs.Snapshot{ID: "job", Kind: kind, Owner: "main", Status: "running"})
				_, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				job := jobs.Snapshot{ID: "job", Kind: kind, Owner: "main", Label: "fixture", Status: tc.status, ExitCode: tc.code, Signal: tc.signal, FinishedAt: "2026-01-01T00:00:00Z"}
				r.publishJob(job)
				m, next, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
				if err != nil || m == nil {
					t.Fatal("missing terminal transition", m, err)
				}
				var view runtimeContext
				if err := json.Unmarshal([]byte(m.Content), &view); err != nil {
					t.Fatal(err)
				}
				want := stateChange{Kind: "job", ID: job.ID, Label: job.Label, From: "running", To: job.Status, ExitCode: job.ExitCode, Signal: job.Signal, FinishedAt: job.FinishedAt}
				if !reflect.DeepEqual(view.Changes, []stateChange{want}) || len(view.Jobs) != 0 || next.jobs[job.ID] != job.Status {
					t.Fatal("canonical state or exit metadata changed", view, next.jobs)
				}
				if m, _, err := r.runtimeContext(context.Background(), "main", r.selection, next); err != nil || m != nil {
					t.Fatal("terminal transition repeated", m, err)
				}
			})
		}
	}
}

func TestRuntimeCommandExitStatusMatchesJobSurfaces(t *testing.T) {
	for _, kind := range []string{"shell", "watch"} {
		for _, code := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/exit_%d", kind, code), func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				seedRuntime(t, r, "Compare job status surfaces")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, cursor, err := r.runtimeContext(ctx, "main", r.selection, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				command := fmt.Sprintf("exit %d", code)
				var id string
				if kind == "shell" {
					id, err = r.Jobs.Start("main", command, r.Workspace.Root, time.Second, true, false, false, capture.OutputOptions{}, "")
				} else {
					dir := filepath.Join(r.Workspace.Root, "run")
					if _, err := watchrun.Run(dir, []string{"/bin/sh", "-c", command}, nil, io.Discard, io.Discard); err != nil {
						t.Fatal(err)
					}
					id, err = r.Jobs.StartWatch("main", dir, false, "")
				}
				if err != nil {
					t.Fatal(err)
				}
				job, err := r.Jobs.Wait(ctx, "main", id, nil)
				if err != nil || job.Status != "completed" || job.ExitCode == nil || *job.ExitCode != code {
					t.Fatal("unexpected command completion", job, err)
				}
				listed := r.Jobs.List("main", true)
				if len(listed) != 1 || listed[0].Status != job.Status || !reflect.DeepEqual(listed[0].ExitCode, job.ExitCode) {
					t.Fatal("job_list disagrees with completion", listed)
				}
				page, err := r.Jobs.Read(ctx, "main", id, jobs.ReadOptions{Limit: 100})
				if err != nil || page["status"] != job.Status || page["exit_code"] != code {
					t.Fatal("job_read disagrees with completion", page, err)
				}
				var notification struct {
					Status   string `json:"status"`
					ExitCode *int   `json:"exit_code"`
				}
				if err := json.Unmarshal([]byte(jobExitContent(job)), &notification); err != nil || notification.Status != job.Status || !reflect.DeepEqual(notification.ExitCode, job.ExitCode) {
					t.Fatal("job_exit disagrees with completion", notification, err)
				}
				m, _, err := r.runtimeContext(ctx, "main", r.selection, cursor)
				if err != nil || m == nil {
					t.Fatal("missing runtime transition", m, err)
				}
				var view runtimeContext
				if err := json.Unmarshal([]byte(m.Content), &view); err != nil {
					t.Fatal(err)
				}
				if len(view.Jobs) != 0 || len(view.Changes) != 1 || view.Changes[0].To != job.Status || !reflect.DeepEqual(view.Changes[0].ExitCode, job.ExitCode) {
					t.Fatal("runtime transition disagrees with completion", view)
				}
			})
		}
	}
}
