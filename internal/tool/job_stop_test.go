package tool

import (
	"context"
	"encoding/json"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/render"
)

func TestJobStopKeepsOutputFreeIdentity(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, label := range []string{"Check results", "", "Shell command"} {
		id, err := m.Start(x.Actor, "printf output", w.Root, 0, true, true, capture.OutputOptions{}, label)
		if err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"job_id": id})
		rec := invoke(t, r, w, x, req, "job_stop", string(args))
		ok(t, rec)
		var result map[string]any
		if err := json.Unmarshal(rec.Result, &result); err != nil {
			t.Fatal(err)
		}
		want := label
		if want == "" {
			want = "printf output"
		}
		if result["kind"] != "shell" || result["label"] != want || result["job_id"] != id || result["status"] == nil {
			t.Fatal(string(rec.Result))
		}
		for _, field := range []string{"stdout", "stderr", "command"} {
			if _, ok := result[field]; ok {
				t.Fatalf("unexpected %s: %s", field, rec.Result)
			}
		}
		md := render.Tool("job_stop", args, rec.Result)
		if md.Summary == "" {
			t.Fatal("missing presentation")
		}
	}
}
