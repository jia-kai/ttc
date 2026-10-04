package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/jobs"
)

func TestUnknownJobHandlesReturnNotFound(t *testing.T) {
	r, w, x, request := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, test := range []struct{ name, args string }{
		{"job_read", `{"job_id":"missing"}`},
		{"job_stop", `{"job_id":"missing"}`},
		{"lsp_query", `{"job_id":"missing","operation":"workspace_symbols","query":""}`},
	} {
		record := invoke(t, r, w, x, request, test.name, test.args)
		var result struct {
			OK    bool
			Error *Error
		}
		if err := json.Unmarshal(record.Result, &result); err != nil || result.OK || result.Error == nil || result.Error.Code != "not_found" {
			t.Fatalf("%s: %s (%v)", test.name, record.Result, err)
		}
	}
}

func TestLSPStrictOperationParameters(t *testing.T) {
	r, w, _, _ := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	tool, ok := r.Get("lsp_query")
	if !ok {
		t.Fatal("lsp_query not registered")
	}
	for _, input := range []string{
		`{"job_id":"a","operation":"hover","path":"x.py","line":1,"column":1,"limit":1}`,
		`{"job_id":"a","operation":"definition","path":"x.py","line":0,"column":1}`,
		`{"job_id":"a","operation":"references","path":"x.py","line":1}`,
		`{"job_id":"a","operation":"workspace_symbols"}`,
		`{"job_id":"a","operation":"workspace_symbols","query":"","path":"x.py"}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","line":1}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","query":"x"}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","limit":501}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","offset":-1}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","timeout_ms":0}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.py","unknown":true}`,
	} {
		if _, err := tool.DecodeCall(1, []byte(input)); err == nil {
			t.Fatal("accepted", input)
		}
	}
	for _, input := range []string{
		`{"job_id":"a","operation":"hover","path":"x.py","line":1,"column":1}`,
		`{"job_id":"a","operation":"workspace_symbols","query":""}`,
		`{"job_id":"a","operation":"document_symbols","path":"x.custom","language_id":"python","offset":0,"limit":500}`,
	} {
		if _, err := tool.DecodeCall(1, []byte(input)); err != nil {
			t.Fatal(input, err)
		}
	}
	shell, _ := r.Get("shell")
	if _, err := shell.DecodeCall(1, []byte(`{"command":"true","protocol":"lsp"}`)); err == nil {
		t.Fatal("accepted foreground LSP")
	}
}

func TestLSPToolLifecycleAndPortableResult(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires python3")
	}
	fixture, err := filepath.Abs("../lsp/testdata/server.py")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(text string) string { return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'" }
	r, w, x, req := toolFixture(t)
	if err := os.WriteFile(filepath.Join(w.Root, "sample.py"), []byte("a😀éz\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	args, _ := json.Marshal(map[string]any{"command": "exec " + quote(python) + " " + quote(fixture), "background": true, "protocol": "lsp"})
	rec := invoke(t, r, w, x, req, "shell", string(args))
	ok(t, rec)
	var launched struct {
		ID   string `json:"job_id"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Result, &launched); err != nil || launched.Kind != "lsp" {
		t.Fatal(string(rec.Result), err)
	}
	args, _ = json.Marshal(map[string]any{"job_id": launched.ID, "operation": "definition", "path": "sample.py", "line": 1, "column": 3, "limit": 1})
	rec = invoke(t, r, w, x, req, "lsp_query", string(args))
	ok(t, rec)
	if !strings.Contains(string(rec.Result), `"start_column":2`) || !strings.Contains(string(rec.Result), `"end_column":4`) {
		t.Fatal(string(rec.Result))
	}
	args, _ = json.Marshal(map[string]any{"job_id": launched.ID, "stream": "stdout"})
	rec = invoke(t, r, w, x, req, "job_read", string(args))
	if !strings.Contains(string(rec.Result), `"code":"invalid_input"`) {
		t.Fatal(string(rec.Result))
	}
	args, _ = json.Marshal(map[string]any{"job_id": launched.ID})
	rec = invoke(t, r, w, x, req, "job_stop", string(args))
	ok(t, rec)
	args, _ = json.Marshal(map[string]any{"job_id": launched.ID, "operation": "hover", "path": "sample.py", "line": 1, "column": 1})
	rec = invoke(t, r, w, x, req, "lsp_query", string(args))
	if !strings.Contains(string(rec.Result), `"code":"job_not_running"`) {
		t.Fatal(string(rec.Result))
	}
}
