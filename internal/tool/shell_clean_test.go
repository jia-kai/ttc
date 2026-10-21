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
