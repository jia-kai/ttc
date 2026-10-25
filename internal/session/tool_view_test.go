package session

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"ttc/internal/historytest"

	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/render"
)

func TestJobReadInspectionPreservesRequestedPageWithoutLiveTails(t *testing.T) {
	for _, arguments := range []string{
		`{"stream":"stderr","grep":"selected"}`,
		`{"stream":"stdout","cursor":"0","limit_bytes":12}`,
		`{"stream":"stdout","grep":"absent"}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.Emit = nil
			seedRuntime(t, r, "Captured job history")
			id, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: "shell", Label: "captured fixture"}, func(_ context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
				if _, err := io.WriteString(stdout, "first-output\nunselected-current-tail\n"); err != nil {
					return jobs.TaskResult{}, err
				}
				_, err := io.WriteString(stderr, "selected diagnostic\nother diagnostic\n")
				return jobs.TaskResult{}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
				t.Fatal(err)
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				t.Fatal(err)
			}
			args["job_id"] = id
			encoded, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			r.Provider = &llm.Script{Responses: []llm.ScriptResponse{
				{Calls: []llm.ToolCall{{ID: "read-page", Name: "job_read", Arguments: encoded}}},
				{Text: "Page read."},
			}}
			if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: "Inspect a captured page"}); err != nil {
				t.Fatal(err)
			}
			var entryID int64
			var result string
			if err := historytest.DB(t, r.Store).QueryRow("SELECT r.entry_id,c.result_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='job_read'").Scan(&entryID, &result); err != nil {
				t.Fatal(err)
			}
			entry, err := r.Store.Entry(entryID)
			if err != nil {
				t.Fatal(err)
			}
			want := render.Tool("job_read", encoded, json.RawMessage(result)).Detail
			for _, reset := range []bool{false, true} {
				if reset {
					if _, err := r.Command("/clear"); err != nil {
						t.Fatal(err)
					}
				}
				got, err := r.Store.ReadInspection(context.Background(), entry.ID)
				if err != nil || got.Text != want || strings.Contains(got.Text, "unselected-current-tail") || strings.Contains(got.Text, "showing tail") {
					t.Fatal("inspector substituted live capture for requested page", got.Text, err)
				}
			}
		})
	}
}

func TestQuietChildShellInspectionIncludesLiveMetadata(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	const actor = "main/child_quiet"
	const command = "sleep 30"
	id, err := r.Jobs.Start(actor, command, r.Workspace.Root, 0, false, true, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	detail := r.JobDetail(id, "")
	for _, want := range []string{command, id, actor, "running", "**Running time:** 0h0m"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("job inspection missing %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "unavailable") || strings.Contains(detail, "Strict:") {
		t.Fatal("inspection invented unavailable shell arguments", detail)
	}
	if _, err := r.Jobs.Stop("main", id); err != nil {
		t.Fatal(err)
	}
	if detail := r.JobDetail(id, ""); !strings.Contains(detail, "cancelled") {
		t.Fatal("inspection did not refresh job status", detail)
	}
}

func TestJobRunningTime(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		status   string
		elapsed  time.Duration
		finished string
		want     string
	}{
		{name: "zero", status: "running", want: "0h0m0s"},
		{name: "seconds", status: "running", elapsed: 59*time.Second + 999*time.Millisecond, want: "0h0m59s"},
		{name: "minute", status: "running", elapsed: time.Minute, want: "0h1m0s"},
		{name: "hour", status: "running", elapsed: time.Hour + 2*time.Minute + 3*time.Second, want: "1h2m3s"},
		{name: "days", status: "running", elapsed: 49*time.Hour + 5*time.Second, want: "49h0m5s"},
		{name: "future", status: "running", elapsed: -time.Second, want: "0h0m0s"},
		{name: "completed", status: "completed", elapsed: 24 * time.Hour, finished: started.Add(time.Hour + 2*time.Minute + 3*time.Second).Format(time.RFC3339Nano), want: "1h2m3s"},
		{name: "cancelled", status: "cancelled", elapsed: time.Hour, finished: started.Add(time.Second).Format(time.RFC3339Nano), want: "0h0m1s"},
		{name: "failed", status: "failed", elapsed: time.Hour, finished: started.Add(2 * time.Second).Format(time.RFC3339Nano), want: "0h0m2s"},
		{name: "missing finish", status: "completed"},
		{name: "invalid finish", status: "failed", finished: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := jobs.Snapshot{Status: tc.status, StartedAt: started.Format(time.RFC3339Nano), FinishedAt: tc.finished}
			got, ok := jobRunningTime(job, started.Add(tc.elapsed))
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("got %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	if got, ok := jobRunningTime(jobs.Snapshot{StartedAt: "invalid"}, started); got != "" || ok {
		t.Fatalf("invalid start: %q, %v", got, ok)
	}
	job := jobs.Snapshot{Status: "completed", StartedAt: started.Add(900 * time.Millisecond).Format(time.RFC3339Nano), FinishedAt: started.Add(1950 * time.Millisecond).Format(time.RFC3339Nano)}
	if got, ok := jobRunningTime(job, started.Add(time.Hour)); got != "0h0m1s" || !ok {
		t.Fatalf("fractional timestamps lost a second: %q, %v", got, ok)
	}
}

func TestInspectionTailDoesNotReduceCapture(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, kind := range []string{"shell", "subagent"} {
		id, err := r.Jobs.StartTask("main", jobs.TaskOptions{Kind: kind, Label: "large output"}, func(ctx context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
			_, err := io.WriteString(stdout, "stdout-prefix\n"+strings.Repeat("o", 12<<10)+"stdout-end")
			if err != nil {
				return jobs.TaskResult{}, err
			}
			_, err = io.WriteString(stderr, "stderr-prefix\n"+strings.Repeat("e", 12<<10)+"stderr-end")
			return jobs.TaskResult{}, err
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := r.Jobs.Wait(context.Background(), "main", id, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		base := render.Tool(kind, []byte(`{"command":"run fixture"}`), result).Detail
		detail := r.JobDetail(id, base)
		if strings.Contains(detail, "prefix") || !strings.Contains(detail, "stdout-end") || !strings.Contains(detail, "stderr-end") || strings.Count(detail, "showing tail, up to 8 KiB") != 2 || strings.Count(detail, "o") > (8<<10)+100 || strings.Count(detail, "e") > (8<<10)+100 {
			t.Fatal("inspection cap/stream labels", detail[:min(len(detail), 100)])
		}
		for _, stream := range []string{"stdout", "stderr"} {
			page, err := r.Jobs.Read(context.Background(), "main", id, jobs.ReadOptions{Stream: stream, Limit: 32 << 10})
			if err != nil || !strings.Contains(page["output"].(string), stream+"-prefix") || len(page["output"].(string)) < 12<<10 {
				t.Fatal("inspect reduced retained capture", stream, err)
			}
		}
	}
}

func TestLSPInspectorShowsStderrWithoutReadingProtocol(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	id, err := r.Jobs.StartLSP("main", "printf 'server diagnostic' >&2", r.Workspace.Root, 0, true, capture.OutputOptions{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Jobs.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	detail := r.JobDetail(id, "base")
	if !strings.Contains(detail, "server diagnostic") || strings.Contains(detail, "stdout") || strings.Contains(detail, "unavailable") {
		t.Fatal(detail)
	}
}

func TestCustomJobInspectionIsMaterializedWithoutHistoricalCallbacks(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Custom inspection history")
	completed := make(chan Event, 1)
	r.Emit = func(event Event) {
		if event.Kind == "job" {
			completed <- event
		}
	}
	var calls atomic.Int64
	id, err := r.Jobs.StartTask("main", jobs.TaskOptions{
		Kind: "custom", Label: "custom task", Background: true,
		Inspector: func(snapshot jobs.Snapshot) jobs.InspectionSpec {
			calls.Add(1)
			return jobs.InspectionSpec{
				Title:   "Custom result",
				Fields:  []jobs.InspectionField{{Name: "Dataset", Value: "training.tsv"}},
				Streams: []jobs.InspectionStream{{Name: "stdout", LimitBytes: 65536, Markdown: true}},
			}
		},
	}, func(_ context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
		_, err := io.WriteString(stdout, "## Custom answer\n\nVerified **evidence**.\n")
		return jobs.TaskResult{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.Jobs.Wait(ctx, "main", id, nil); err != nil {
		t.Fatal(err)
	}
	var completion Event
	select {
	case completion = <-completed:
	case <-ctx.Done():
		t.Fatal("missing saved completion")
	}
	for _, detail := range []string{r.JobDetail(id, ""), r.JobDetail(id, "Original tool parameters\n")} {
		for _, want := range []string{"Custom result", "**Dataset:** training.tsv", "## Custom answer", "**Running time:**"} {
			if !strings.Contains(detail, want) {
				t.Fatalf("custom inspection missing %q: %s", want, detail)
			}
		}
		if strings.Contains(detail, "```text") || strings.Contains(detail, "showing tail") {
			t.Fatalf("custom Markdown was fenced: %s", detail)
		}
	}
	before := calls.Load()
	entry, err := r.Store.Entry(completion.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Store.ReadInspection(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Command("/clear"); err != nil {
		t.Fatal(err)
	}
	second, err := r.Store.ReadInspection(context.Background(), entry.ID)
	if err != nil || second.Text != first.Text || !strings.Contains(second.Text, "training.tsv") || calls.Load() != before {
		t.Fatalf("historical inspection invoked live provider or changed: %v, calls %d -> %d\n%s", err, before, calls.Load(), second.Text)
	}
}

func TestBTWInspectionDisclosesTruncatedFeedbackInSavedDetail(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Large aside feedback")
	saved := make(chan Event, 1)
	r.Emit = func(event Event) {
		if event.Kind == "btw_result" {
			saved <- event
		}
	}
	id, err := r.Jobs.StartTask("main/btw_fixture", jobs.TaskOptions{
		Kind: "btw", Label: "Explain evidence", Background: true, Inspector: btwInspector,
	}, func(_ context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
		if _, err := io.WriteString(stdout, "## Aside answer\n\nVerified **evidence**.\n"); err != nil {
			return jobs.TaskResult{}, err
		}
		_, err := io.WriteString(stderr, "omitted-feedback-prefix\n"+strings.Repeat("feedback\n", 10000)+"retained-feedback-end")
		return jobs.TaskResult{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.Jobs.Wait(ctx, "main", id, nil); err != nil {
		t.Fatal(err)
	}
	var event Event
	select {
	case event = <-saved:
	case <-ctx.Done():
		t.Fatal("missing aside completion")
	}
	entry, err := r.Store.Entry(event.EntryID)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := r.Store.ReadInspection(context.Background(), entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inspected := range []string{r.JobDetail(id, ""), detail.Text} {
		for _, want := range []string{"## Aside answer", "**Tool feedback**", "Retained stderr output was truncated.", "retained-feedback-end", "```text"} {
			if !strings.Contains(inspected, want) {
				t.Fatalf("aside inspection missing %q", want)
			}
		}
		if strings.Contains(inspected, "omitted-feedback-prefix") || strings.Contains(inspected, "Retained stdout output was truncated.") {
			t.Fatal("aside capture truncation was misreported")
		}
	}
}

func TestJobInspectionDisclosesWhollyEvictedOutput(t *testing.T) {
	inspection := jobs.Inspection{Output: []jobs.InspectionOutput{
		{InspectionStream: jobs.InspectionStream{Name: "stdout", LimitBytes: 8192, Markdown: true}, Truncated: true},
		{InspectionStream: jobs.InspectionStream{Name: "stderr", LimitBytes: 8192}, Truncated: true},
	}}
	detail := jobInspectionDetail(inspection, "Original parameters\n", time.Now())
	for _, stream := range []string{"stdout", "stderr"} {
		if !strings.Contains(detail, "Retained "+stream+" output was truncated.") {
			t.Fatal("wholly evicted capture was silently omitted", detail)
		}
	}
	if strings.Contains(detail, "```text") {
		t.Fatal("inspection invented output for empty captures", detail)
	}
}
