package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/jobs"
)

func TestShellCleanToolAndRawOverride(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, raw := range []bool{false, true} {
		args, _ := json.Marshal(map[string]any{"command": "printf '\\033[31mold\\rnew\\033[K\\033[0m\\n'", "strip_ansi": !raw, "output_mode": " HEAD_TAIL "})
		record := invoke(t, r, w, x, req, "shell", string(args))
		var result jobs.Snapshot
		if err := json.Unmarshal(record.Result, &result); err != nil || result.Status != "completed" {
			t.Fatal(string(record.Result), err)
		}
		if raw {
			if !strings.Contains(result.Stdout, "\x1b[") {
				t.Fatal(result)
			}
		} else if result.Stdout != "new\n" {
			t.Fatal(result)
		}
		readArgs, _ := json.Marshal(map[string]any{"job_id": result.ID, "stream": " STDOUT ", "strip_ansi": false})
		page := invoke(t, r, w, x, req, "job_read", string(readArgs))
		var output map[string]any
		if err := json.Unmarshal(page.Result, &output); err != nil || output["ok"] != true || !strings.Contains(output["output"].(string), "\x1b[") {
			t.Fatal(string(page.Result), err)
		}
	}
}

func TestJobReadGrepCreatesPaginatedSnapshot(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	record := invoke(t, r, w, x, req, "shell", `{"command":"printf 'skip\\nhit one\\nhit two\\n'"}`)
	var source jobs.Snapshot
	if err := json.Unmarshal(record.Result, &source); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"job_id": source.ID, "grep": "hit", "limit_bytes": 8})
	record = invoke(t, r, w, x, req, "job_read", string(args))
	var first struct {
		ID     string          `json:"job_id"`
		Kind   string          `json:"kind"`
		Output string          `json:"output"`
		Next   string          `json:"next_cursor"`
		Origin jobs.GrepOrigin `json:"grep_origin"`
	}
	if err := json.Unmarshal(record.Result, &first); err != nil || first.ID == source.ID || first.Kind != "grep" || first.Output != "hit one\n" || first.Origin.SourceJobID != source.ID {
		t.Fatal(string(record.Result), err)
	}
	args, _ = json.Marshal(map[string]any{"job_id": first.ID, "cursor": first.Next})
	record = invoke(t, r, w, x, req, "job_read", string(args))
	var last map[string]any
	if err := json.Unmarshal(record.Result, &last); err != nil || last["output"] != "hit two\n" || last["next_cursor"] != nil {
		t.Fatal(string(record.Result), err)
	}
	args, _ = json.Marshal(map[string]any{"job_id": source.ID, "grep": "hit", "cursor": "0"})
	record = invoke(t, r, w, x, req, "job_read", string(args))
	if !strings.Contains(string(record.Result), "grep requires an empty cursor") {
		t.Fatal(string(record.Result))
	}
}
