package session

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

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
			id, err := r.Jobs.StartTask("main", "shell", "captured fixture", false, false, func(_ context.Context, stdout, stderr io.Writer) error {
				if _, err := io.WriteString(stdout, "first-output\nunselected-current-tail\n"); err != nil {
					return err
				}
				_, err := io.WriteString(stderr, "selected diagnostic\nother diagnostic\n")
				return err
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
			if err := r.Run(&llm.Message{Role: "user", Content: "Inspect a captured page"}); err != nil {
				t.Fatal(err)
			}
			var entryID int64
			var result string
			if err := r.Store.DB.QueryRow("SELECT r.entry_id,c.result_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='job_read'").Scan(&entryID, &result); err != nil {
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
				got, err := r.Store.Inspect(entry)
				if err != nil || got != want || strings.Contains(got, "unselected-current-tail") || strings.Contains(got, "showing tail") {
					t.Fatal("inspector substituted live capture for requested page", got, err)
				}
			}
		})
	}
}

func TestQuietChildShellInspectionIncludesLiveMetadata(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	const actor = "main/child_quiet"
	const command = "sleep 30"
	id, err := r.Jobs.Start(actor, command, r.Workspace.Root, 0, false, true, false)
	if err != nil {
		t.Fatal(err)
	}
	detail := r.JobDetail(id, "")
	for _, want := range []string{command, id, actor, "running"} {
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

func TestInspectionTailDoesNotReduceCapture(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	for _, kind := range []string{"shell", "subagent"} {
		id, err := r.Jobs.StartTask("main", kind, "large output", false, false, func(ctx context.Context, stdout, stderr io.Writer) error {
			_, err := io.WriteString(stdout, "stdout-prefix\n"+strings.Repeat("o", 12<<10)+"stdout-end")
			if err != nil {
				return err
			}
			_, err = io.WriteString(stderr, "stderr-prefix\n"+strings.Repeat("e", 12<<10)+"stderr-end")
			return err
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
		if strings.Contains(detail, "prefix") || !strings.Contains(detail, "stdout-end") || !strings.Contains(detail, "stderr-end") || strings.Count(detail, "showing tail, up to 8 KiB") != 2 || strings.Count(detail, "o") > inspectionTailBytes+100 || strings.Count(detail, "e") > inspectionTailBytes+100 {
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
	id, err := r.Jobs.StartLSP("main", "printf 'server diagnostic' >&2", r.Workspace.Root, 0, true, false)
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
