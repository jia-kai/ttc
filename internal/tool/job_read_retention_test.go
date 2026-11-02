package tool

import (
	"context"
	"encoding/json"
	"testing"

	"ttc/internal/jobs"
)

func TestJobReadEOFOvershootDoesNotReportLoss(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	started := invoke(t, r, w, x, req, "shell", `{"command":"printf 'short\\n'"}`)
	var job jobs.Snapshot
	if err := json.Unmarshal(started.Result, &job); err != nil || job.Status != "completed" {
		t.Fatal(string(started.Result), err)
	}
	args, err := json.Marshal(map[string]any{"job_id": job.ID, "cursor": "eof:-100:bytes"})
	if err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "job_read", string(args))
	var result struct {
		OK        bool   `json:"ok"`
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(record.Result, &result); err != nil || !result.OK || result.Output != "short\n" || result.Truncated {
		t.Fatal(string(record.Result), err)
	}
}
