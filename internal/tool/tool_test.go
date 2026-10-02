package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/scratch"
	"scicode/internal/workspace"
	"strings"
	"testing"
)

func toolFixture(t *testing.T) (*Registry, *workspace.Manager, Execution, int64) {
	t.Helper()
	if _, e := scratch.Verify(); e != nil {
		t.Fatal(e)
	}
	s, e := history.Open(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	w, e := workspace.Open(t.TempDir(), s)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { w.Close() })
	selection := provider.Selection{Model: provider.ScriptModel()}
	id := history.NewID("session")
	turn, _, e := s.StartSession(id, w.Root, selection, provider.Message{Role: "user", Content: "Exercise file tools"})
	if e != nil {
		t.Fatal(e)
	}
	req, e := s.StartRequest(id, turn, "main", "coding", selection)
	if e != nil {
		t.Fatal(e)
	}
	r := NewRegistry()
	AddFiles(r, w)
	return r, w, Execution{SessionID: id, Actor: "main"}, req
}
func invoke(t *testing.T, r *Registry, w *workspace.Manager, x Execution, req int64, name, args string) Record {
	t.Helper()
	var turn string
	if e := w.Store.DB.QueryRow("SELECT coalesce(turn_id,'') FROM model_requests WHERE id=?", req).Scan(&turn); e != nil {
		t.Fatal(e)
	}
	id, e := w.Store.CallIntent(x.SessionID, turn, x.Actor, req, provider.ToolCall{ID: history.NewID("p"), Name: name, Arguments: []byte(args)})
	if e != nil {
		t.Fatal(e)
	}
	x.CallID = id
	return r.Invoke(context.Background(), x, name, []byte(args))
}
func ok(t *testing.T, r Record) {
	t.Helper()
	var v struct {
		OK bool `json:"ok"`
	}
	if e := json.Unmarshal(r.Result, &v); e != nil || !v.OK {
		t.Fatal(string(r.Result), e)
	}
}
func TestStrictCodecsFilesPagingEditAndPatch(t *testing.T) {
	r, w, x, req := toolFixture(t)
	rec := invoke(t, r, w, x, req, "write", `{"path":"x.txt","content":"a\nb\nc\n"}`)
	ok(t, rec)
	tool, _ := r.Get("write")
	version, b, e := rec.Encode()
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := tool.DecodeRecord(version, b)
	if e != nil || string(decoded.Result) != string(rec.Result) {
		t.Fatal(decoded, e)
	}
	if _, e = tool.DecodeRecord(2, b); e == nil {
		t.Fatal("accepted codec version")
	}
	for _, args := range []string{`{"path":"x","content":"y","unknown":true}`, `{"path":"x"}`, `null`, `{"path":"x","content":"y"} {}`} {
		if _, e = tool.DecodeCall(1, []byte(args)); e == nil {
			t.Fatal("accepted", args)
		}
	}
	rec = invoke(t, r, w, x, req, "read", `{"path":"x.txt","offset":2,"limit":1}`)
	ok(t, rec)
	var page struct {
		Content string
		Next    int `json:"next_offset"`
	}
	json.Unmarshal(rec.Result, &page)
	if page.Content != "b\n" || page.Next != 3 {
		t.Fatal(string(rec.Result))
	}
	rec = invoke(t, r, w, x, req, "edit", `{"path":"x.txt","old_text":"b","new_text":"B"}`)
	ok(t, rec)
	patch := `*** Begin Patch
*** Update File: x.txt
@@
 a
-B
+beta
 c
*** Add File: d/new.txt
+new
*** End Patch`
	args, _ := json.Marshal(map[string]string{"patch_text": patch})
	ok(t, invoke(t, r, w, x, req, "patch", string(args)))
	data, _ := os.ReadFile(filepath.Join(w.Root, "x.txt"))
	if string(data) != "a\nbeta\nc\n" {
		t.Fatal(string(data))
	}
	move := `*** Begin Patch
*** Update File: d/new.txt
*** Move to: moved.txt
@@
-new
+moved
*** End Patch`
	args, _ = json.Marshal(map[string]string{"patch_text": move})
	ok(t, invoke(t, r, w, x, req, "patch", string(args)))
	if _, e = os.Stat(filepath.Join(w.Root, "d/new.txt")); !os.IsNotExist(e) {
		t.Fatal("move kept source")
	}
}
func TestSearchAndFetch(t *testing.T) {
	r, w, x, req := toolFixture(t)
	os.WriteFile(filepath.Join(w.Root, "keep.txt"), []byte("needle\nother\n"), 0600)
	os.WriteFile(filepath.Join(w.Root, "skip.txt"), []byte("needle"), 0600)
	os.WriteFile(filepath.Join(w.Root, ".gitignore"), []byte("skip.txt\n"), 0600)
	rec := invoke(t, r, w, x, req, "glob", `{"pattern":"**/*.txt"}`)
	ok(t, rec)
	var paths struct{ Paths []string }
	json.Unmarshal(rec.Result, &paths)
	// Native rg glob inclusions override ignore rules for matching paths.
	if len(paths.Paths) != 2 || paths.Paths[0] != "keep.txt" || paths.Paths[1] != "skip.txt" {
		t.Fatal(string(rec.Result))
	}
	rec = invoke(t, r, w, x, req, "grep", `{"pattern":"needle","literal":true}`)
	ok(t, rec)
	var matches struct{ Matches []any }
	json.Unmarshal(rec.Result, &matches)
	if len(matches.Matches) != 1 {
		t.Fatal(string(rec.Result))
	}
	// A deterministic transport keeps this HTML-processing unit test independent
	// of socket permissions. The separate mock demo covers actual HTTP fetching.
	AddWeb(r, &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader("<p>hello</p><script>bad</script><p>world</p>")), Request: req}, nil
	})}, WebSearchConfig{})
	args, _ := json.Marshal(map[string]any{"url": "https://mock.test", "max_chars": 5})
	rec = invoke(t, r, w, x, req, "web_fetch", string(args))
	ok(t, rec)
	var page struct {
		Content   string
		Truncated bool
	}
	json.Unmarshal(rec.Result, &page)
	if page.Content != "hello" || !page.Truncated {
		t.Fatal(string(rec.Result))
	}
}

func TestLongReadMakesProgressOrFailsAndMovePreservesMode(t *testing.T) {
	r, w, x, req := toolFixture(t)
	path := filepath.Join(w.Root, "long")
	os.WriteFile(path, []byte(strings.Repeat("x", 40001)), 0600)
	rec := invoke(t, r, w, x, req, "read", `{"path":"long"}`)
	if !strings.Contains(string(rec.Result), "line_too_long") {
		t.Fatal(string(rec.Result))
	}
	os.WriteFile(filepath.Join(w.Root, "executable"), []byte("old\n"), 0755)
	args, _ := json.Marshal(map[string]string{"patch_text": "*** Begin Patch\n*** Update File: executable\n*** Move to: moved\n@@\n-old\n+new\n*** End Patch"})
	ok(t, invoke(t, r, w, x, req, "patch", string(args)))
	st, e := os.Stat(filepath.Join(w.Root, "moved"))
	if e != nil || st.Mode().Perm() != 0755 {
		t.Fatal(st, e)
	}
}
func TestEmptySchemasAreValidObjectsAndArrays(t *testing.T) {
	r := NewRegistry()
	Register(r, "empty", "empty", nil, nil, func(a struct{}) error { return nil }, func(context.Context, Execution, struct{}) (any, error) { return nil, nil })
	for _, d := range r.Definitions() {
		var schema map[string]any
		if e := json.Unmarshal(d.Parameters, &schema); e != nil {
			t.Fatal(e)
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Fatal(string(d.Parameters))
		}
		if _, ok := schema["required"].([]any); !ok {
			t.Fatal(string(d.Parameters))
		}
	}
}

type fetchTransport func(*http.Request) (*http.Response, error)

func (f fetchTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
