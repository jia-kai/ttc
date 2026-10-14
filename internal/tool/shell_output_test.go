package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/jobs"
)

func TestShellOutputArgumentsAndSelection(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	shell, _ := r.Get("shell")
	for _, args := range []string{
		`{"command":"true","output_mode":"all"}`,
		`{"command":"true","output_bytes":0}`,
		`{"command":"true","output_bytes":-1}`,
		`{"command":"true","output_bytes":8193}`,
	} {
		if _, err := shell.DecodeCall(1, []byte(args)); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, args := range []string{
		`{"command":"true","output_mode":"head"}`,
		`{"command":"true","output_mode":"tail","output_bytes":1}`,
		`{"command":"true","output_mode":"head_tail","output_bytes":8192}`,
	} {
		if _, err := shell.DecodeCall(1, []byte(args)); err != nil {
			t.Fatal(args, err)
		}
	}
	record := invoke(t, r, w, x, req, "shell", `{"command":"printf 0123456789; printf abcdefghij >&2","output_mode":"head_tail","output_bytes":8}`)
	var result jobs.Snapshot
	if err := json.Unmarshal(record.Result, &result); err != nil || result.Stdout != "0189" || result.Stderr != "abij" || !result.Truncated || result.StdoutSelection == nil || result.StdoutSelection.OmittedBytes != 6 {
		t.Fatal(string(record.Result), err)
	}
}

func TestShellOutputCeilingFitsEncodedResult(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, tc := range []struct {
		command string
		want    string
		omitted int64
	}{
		{"head -c 8192 /dev/zero", strings.Repeat("\x00", capture.MaxOutputBytes), 0},
		{"head -c 10000 /dev/zero | tr '\\000' x", strings.Repeat("x", capture.MaxOutputBytes), 10000 - capture.MaxOutputBytes},
	} {
		args, err := json.Marshal(map[string]any{"command": tc.command, "output_bytes": capture.MaxOutputBytes})
		if err != nil {
			t.Fatal(err)
		}
		record := invoke(t, r, w, x, req, "shell", string(args))
		var result jobs.Snapshot
		if err := json.Unmarshal(record.Result, &result); err != nil || result.Status != "completed" || result.Stdout != tc.want || result.StdoutSelection == nil || result.StdoutSelection.OmittedBytes != tc.omitted || result.StdoutSelection.LostPrefixBytes != 0 || len(record.Result) > 64<<10 {
			t.Fatalf("encoded result length=%d status=%q: %v", len(record.Result), result.Status, err)
		}
	}
}
