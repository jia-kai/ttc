package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/jobs"
)

func TestWatchLabelNormalization(t *testing.T) {
	for _, input := range []string{"unchanged", " line\nbreak\t ", strings.Repeat("界", 30), "\x1b\x00"} {
		label := input
		warnings := normalizeWatchLabel(&label)
		if len(label) > 64 || !utf8.ValidString(label) || strings.ContainsAny(label, "\n\r\t\x1b\x00") {
			t.Fatalf("unsafe normalized label %q", label)
		}
		if (len(warnings) != 0) != (label != input) {
			t.Fatal(input, label, warnings)
		}
		if len(warnings) != 0 && (warnings[0].Requested != input || warnings[0].Effective != label) {
			t.Fatal(warnings)
		}
	}
	r, w, _, _ := toolFixture(t)
	manager := jobs.New(context.Background(), nil)
	defer manager.Close()
	AddWatch(r, manager, w)
	AddTmuxWatchRun(r, manager, w)
	for _, name := range []string{"watch", "tmux_watch_run"} {
		codec, _ := r.Get(name)
		arguments := json.RawMessage(`{"command":"true","record_dir":"record","label":"line\nbreak"}`)
		if name == "watch" {
			arguments = json.RawMessage(`{"record_dir":"record","label":"line\nbreak"}`)
		}
		call, err := codec.DecodeCall(1, arguments)
		if err != nil || len(call.InputWarnings()) != 1 {
			t.Fatal(name, err, call)
		}
		_, effective, err := call.Encode()
		var fields map[string]any
		if err != nil || json.Unmarshal(effective, &fields) != nil || fields["label"] != "line break" {
			t.Fatal(name, string(effective), err)
		}
	}
}

func TestNarrowEnumNormalization(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		changed     bool
	}{
		{" HTML ", "html", true}, {"html", "html", false}, {" Html-ish ", " Html-ish ", false}, {" ", " ", false},
	} {
		value := tc.input
		warnings := NormalizeEnum("format", &value, "html", "text")
		if value != tc.want || (len(warnings) > 0) != tc.changed {
			t.Fatalf("%q => %q, %+v", tc.input, value, warnings)
		}
		if len(warnings) > 0 && (warnings[0].Requested != tc.input || warnings[0].Effective != tc.want || warnings[0].Code != "input_normalized") {
			t.Fatal(warnings)
		}
	}
}

func TestLSPRedundantArgumentsAndPagination(t *testing.T) {
	r, w, _, _ := toolFixture(t)
	addLSP(r, nil, w)
	codec, _ := r.Get("lsp_query")
	for _, input := range []string{
		`{"job_id":" a ","operation":" HOVER ","path":" x ","line":1,"column":1,"query":"","language_id":"","offset":0,"limit":100}`,
		`{"job_id":"a","operation":"workspace_symbols","query":"","path":"","language_id":""}`,
		`{"job_id":"a","operation":"document_symbols","path":"x","offset":-7}`,
	} {
		call, err := codec.DecodeCall(1, []byte(input))
		if err != nil {
			t.Fatal(input, err)
		}
		if len(call.InputWarnings()) == 0 {
			t.Fatal("missing warnings", input)
		}
		_, encoded, _ := call.Encode()
		if strings.Contains(input, `"job_id":" a "`) && (!strings.Contains(string(encoded), `"job_id":" a "`) || !strings.Contains(string(encoded), `"path":" x "`)) {
			t.Fatal("trimmed identifier/path", string(encoded))
		}
	}
	for _, input := range []string{
		`{"job_id":"a","operation":"hover","path":"x","line":0,"column":1}`,
		`{"job_id":"a","operation":"workspace_symbols","query":"","path":" "}`,
		`{"job_id":"a","operation":"hover","path":"x","line":1,"column":1,"offset":1}`,
		`{"job_id":"a","operation":"document_symbols","path":"x","query":" "}`,
		`{"job_id":"a","operation":"document_symbols","path":"x","line":0}`,
	} {
		if _, err := codec.DecodeCall(1, []byte(input)); err == nil {
			t.Fatal("accepted", input)
		}
	}
}

func TestWebRetainedFormatAndDefaultContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	defer server.Close()
	r := NewRegistry()
	AddWeb(r, server.Client(), WebSearchConfig{})
	record := r.Invoke(context.Background(), Execution{}, "web_fetch", json.RawMessage(`{"url":"`+server.URL+`","format":" TEXT ","offset":-1,"context_lines":0}`))
	var initial struct {
		DocumentID string `json:"document_id"`
		Warnings   []InputWarning
	}
	if err := json.Unmarshal(record.Result, &initial); err != nil || initial.DocumentID == "" || len(initial.Warnings) != 2 {
		t.Fatal(string(record.Result), err)
	}
	for _, format := range []string{"text", " TEXT ", "html"} {
		args := `{"document_id":"` + initial.DocumentID + `","format":"` + format + `","context_lines":0}`
		rec := r.Invoke(context.Background(), Execution{}, "web_fetch", json.RawMessage(args))
		var result struct{ Error *Error }
		_ = json.Unmarshal(rec.Result, &result)
		if (result.Error != nil) != (format == "html") {
			t.Fatal(args, string(rec.Result))
		}
		if string(rec.Arguments) != args {
			t.Fatal("original arguments changed")
		}
	}
	codec, _ := r.Get("web_fetch")
	for _, suffix := range []string{`,"context_lines":1`, `,"pattern":"["`, `,"unknown":false`, `,"offset":1.0`} {
		if _, err := codec.DecodeCall(1, []byte(`{"url":"`+server.URL+`"`+suffix+`}`)); err == nil {
			t.Fatal("accepted", suffix)
		}
	}
}

func TestVerifiedNoopEdit(t *testing.T) {
	r, w, x, req := toolFixture(t)
	path := filepath.Join(w.Root, "same")
	if err := os.WriteFile(path, []byte("one two two"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := invoke(t, r, w, x, req, "edit", `{"path":"same","old_text":"one","new_text":"one"}`)
	var result struct {
		Unchanged    bool
		Replacements int
		Error        *Error
	}
	_ = json.Unmarshal(rec.Result, &result)
	if !result.Unchanged || result.Replacements != 0 || result.Error != nil {
		t.Fatal(string(rec.Result))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("no-op rewrote the file")
	}
	for _, old := range []string{"missing", "two"} {
		rec = invoke(t, r, w, x, req, "edit", `{"path":"same","old_text":"`+old+`","new_text":"`+old+`"}`)
		if !strings.Contains(string(rec.Result), "ambiguous_match") {
			t.Fatal(string(rec.Result))
		}
	}
	rec = invoke(t, r, w, x, req, "edit", `{"path":"same","old_text":"two","new_text":"two","replace_all":true}`)
	if !strings.Contains(string(rec.Result), `"unchanged":true`) {
		t.Fatal(string(rec.Result))
	}
	rec = invoke(t, r, w, x, req, "read", `{"path":"same","offset":-5}`)
	if !strings.Contains(string(rec.Result), `"start_line":1`) || !strings.Contains(string(rec.Result), "input_clipped") {
		t.Fatal(string(rec.Result))
	}
}
