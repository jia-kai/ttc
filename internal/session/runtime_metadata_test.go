package session

import (
	"context"
	"strings"
	"testing"

	"ttc/internal/jobs"
)

func TestRuntimeJobsExcludeCapturedOutputAndSelection(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.publishedJobs["job_fixture"] = jobs.Snapshot{ID: "job_fixture", Kind: "shell", Owner: "main", Label: "Quick check", Status: "running", Stdout: "stdout-marker", Stderr: "stderr-marker", Truncated: true}
	message, cursor, err := r.runtimeContext(context.Background(), "main", r.selection, contextCursor{})
	if err != nil || message == nil {
		t.Fatal(message, err)
	}
	for _, field := range []string{`"stdout"`, `"stderr"`, `"truncated"`, "stdout-marker", "stderr-marker"} {
		if strings.Contains(message.Content, field) {
			t.Fatal(message.Content)
		}
	}
	v := r.publishedJobs["job_fixture"]
	v.Stdout = "changed output"
	r.publishedJobs[v.ID] = v
	next, _, err := r.runtimeContext(context.Background(), "main", r.selection, cursor)
	if err != nil || next != nil {
		t.Fatal(next, err)
	}
}
