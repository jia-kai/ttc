package tool

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"
	"ttc/internal/jobs"
	"ttc/internal/watchrun"
)

func TestWatchTool(t *testing.T) {
	r, w, x, _ := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddWatch(r, m, w)
	tool, _ := r.Get("watch")
	for _, args := range []string{`{}`, `{"record_dir":""}`, `{"record_dir":"x","unknown":true}`} {
		if _, err := tool.DecodeCall(1, []byte(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	dir := filepath.Join(w.Root, "run")
	if _, err := watchrun.Run(dir, []string{"/bin/sh", "-c", "printf watched; exit 4"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	rec := r.Invoke(context.Background(), x, "watch", []byte(`{"record_dir":"run","label":"recheck","wake_on_exit":false}`))
	var launch jobs.Snapshot
	if err := json.Unmarshal(rec.Result, &launch); err != nil || launch.ID == "" {
		t.Fatalf("launch %s %v", rec.Result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	view, err := m.Wait(ctx, x.Actor, launch.ID, nil)
	if err != nil || view.Kind != "watch" || view.Status != "completed" || view.Stdout != "watched" || view.ExitCode == nil || *view.ExitCode != 4 || view.WakeOnExit || view.Label != "recheck" {
		t.Fatalf("view %+v %v", view, err)
	}
	rec = r.Invoke(ctx, x, "watch", []byte(`{"record_dir":"missing"}`))
	var result struct {
		OK bool `json:"ok"`
	}
	if err = json.Unmarshal(rec.Result, &result); err != nil || result.OK {
		t.Fatalf("missing dir %s %v", rec.Result, err)
	}
}
