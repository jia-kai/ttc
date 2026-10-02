package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"scicode/internal/jobs"
)

func TestShellTimeoutValidationAndForegroundEnforcement(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	shell, _ := r.Get("shell")
	for _, args := range []string{
		`{"command":"true","timeout_ms":0}`,
		`{"command":"true","timeout_ms":-1}`,
		`{"command":"true","timeout_ms":86400001,"background":true}`,
	} {
		if _, err := shell.DecodeCall(1, []byte(args)); err == nil {
			t.Fatal("accepted invalid timeout", args)
		}
	}
	for _, args := range []string{
		`{"command":"true"}`,
		`{"command":"true","timeout_ms":1}`,
		`{"command":"true","timeout_ms":86400000}`,
		`{"command":"true","background":true}`,
		`{"command":"true","timeout_ms":0,"background":true}`,
		`{"command":"true","timeout_ms":1,"background":true}`,
	} {
		if _, err := shell.DecodeCall(1, []byte(args)); err != nil {
			t.Fatal(args, err)
		}
	}
	record := invoke(t, r, w, x, req, "shell", `{"command":"sleep 5; printf should-not-run","timeout_ms":20}`)
	var result jobs.Snapshot
	if err := json.Unmarshal(record.Result, &result); err != nil || result.Status != "cancelled" || strings.Contains(result.Stdout, "should-not-run") {
		t.Fatal(string(record.Result), err)
	}
}

func TestBackgroundShellWithoutTimeoutOutlivesForegroundCancellation(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, timeout := range []string{"", `,"timeout_ms":0`} {
		args := `{"command":"sleep 0.05; printf completed","background":true` + timeout + `}`
		record := invoke(t, r, w, x, req, "shell", args)
		var started jobs.Snapshot
		if err := json.Unmarshal(record.Result, &started); err != nil || started.ID == "" {
			t.Fatal(string(record.Result), err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := m.Wait(ctx, "main", started.ID, nil); err != nil {
			t.Fatal(err)
		}
		finished, err := m.Wait(context.Background(), "main", started.ID, nil)
		if err != nil || finished.Status != "completed" || finished.Stdout != "completed" {
			t.Fatal(finished, err)
		}
	}
}
