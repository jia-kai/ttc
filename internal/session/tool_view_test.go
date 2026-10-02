package session

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"scicode/internal/jobs"
	"scicode/internal/render"
)

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
