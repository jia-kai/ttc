package session

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/jobs"
	"ttc/internal/render"
	"ttc/internal/watchrun"
)

func TestWatchInspectorExpandsCapturedTails(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	dir := filepath.Join(r.Workspace.Root, "external-record")
	if _, err := watchrun.Run(dir, []string{"/bin/sh", "-c", "printf stdout-prefix; printf '%012000d' 0; printf stdout-end; printf stderr-end >&2"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	id, err := r.Jobs.StartWatch("main", dir, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := r.Jobs.Wait(ctx, "main", id, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := captureJobID("watch", result); got != id {
		t.Fatalf("watch inspector did not identify capture: %q", got)
	}
	if got := captureJobID("job_read", result); got != "" {
		t.Fatalf("page inspector incorrectly expanded capture: %q", got)
	}
	base := render.InspectedTool("watch", []byte(`{"record_dir":"external-record"}`), result).Detail
	detail := r.JobDetail(captureJobID("watch", result), base)
	for _, inspected := range []string{detail, r.JobDetail(id, "")} {
		for _, want := range []string{"**Record dir:**", dir, "**Command:**", "printf stdout-prefix", "**Running time:**", "0h0m"} {
			if !strings.Contains(inspected, want) {
				t.Fatalf("watch inspection missing %q: %s", want, inspected)
			}
		}
		if strings.Contains(inspected, "**Label:**") {
			t.Fatalf("watch inspection still shows label: %s", inspected)
		}
	}
	if !strings.Contains(detail, "stdout-end") || !strings.Contains(detail, "stderr-end") || strings.Count(detail, "showing tail, up to 8 KiB") != 2 {
		t.Fatalf("watch inspection did not expand captured tails: %s", detail)
	}
	page, err := r.Jobs.Read(ctx, "main", id, jobs.ReadOptions{Stream: "stdout", Limit: 32 << 10})
	if err != nil || !strings.Contains(page["output"].(string), "stdout-prefix") {
		t.Fatalf("inspection discarded retained prefix: %+v %v", page, err)
	}
}
