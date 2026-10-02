package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"scicode/internal/provider"
	"scicode/internal/provider/openai"
	"scicode/internal/tool"
)

// pipeListener exercises net/http's actual request parsing, connection lifecycle
// and streaming flushes without a TCP bind or access to any external network.
type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}
type pipeAddr string

func (a pipeAddr) Network() string     { return "pipe" }
func (a pipeAddr) String() string      { return string(a) }
func (l *pipeListener) Addr() net.Addr { return pipeAddr("mock.invalid") }
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	case <-l.done:
		_ = client.Close()
		_ = server.Close()
		return nil, net.ErrClosed
	}
}

func mockHTTPClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	l := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	transport := &http.Transport{DialContext: l.dial, ForceAttemptHTTP2: false}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		_ = server.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("HTTP mock server did not join")
		}
	})
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

type mockCall struct {
	name string
	args any
}

func streamMockResponse(w http.ResponseWriter, identity string, step int, calls []mockCall, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flush := w.(http.Flusher)
	frame := func(value any) {
		data, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flush.Flush()
	}
	arguments := make([]string, len(calls))
	callIDs := make([]string, len(calls))
	seenNames := map[string]bool{}
	for index, call := range calls {
		data, _ := json.Marshal(call.args)
		arguments[index] = string(data)
		callIDs[index] = fmt.Sprintf("%s%d_%s", identity, step, call.name)
		if seenNames[call.name] {
			callIDs[index] += fmt.Sprintf("_%d", index)
		}
		seenNames[call.name] = true
		item := map[string]any{"type": "function_call", "id": fmt.Sprintf("item_%s_%d_%d", identity, step, index), "call_id": callIDs[index], "name": call.name, "arguments": ""}
		frame(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
	}
	// Announce all names, then interleave incomplete argument chunks by item ID.
	for part := 0; part < 2; part++ {
		for index, argument := range arguments {
			mid := len(argument) / 2
			for !utf8.ValidString(argument[:mid]) {
				mid--
			}
			delta := argument[:mid]
			if part == 1 {
				delta = argument[mid:]
			}
			frame(map[string]any{"type": "response.function_call_arguments.delta", "item_id": fmt.Sprintf("item_%s_%d_%d", identity, step, index), "output_index": index, "delta": delta})
		}
	}
	for index, call := range calls {
		frame(map[string]any{"type": "response.function_call_arguments.done", "output_index": index, "item_id": fmt.Sprintf("item_%s_%d_%d", identity, step, index), "arguments": arguments[index]})
		frame(map[string]any{"type": "response.output_item.done", "output_index": index, "item": map[string]any{"type": "function_call", "id": fmt.Sprintf("item_%s_%d_%d", identity, step, index), "call_id": callIDs[index], "name": call.name, "arguments": arguments[index], "status": "completed"}})
	}
	if text != "" {
		frame(map[string]any{"type": "response.output_text.delta", "delta": text})
		frame(map[string]any{"type": "response.output_item.done", "output_index": len(calls), "item": map[string]any{"id": fmt.Sprintf("msg_%s_%d", identity, step), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}})
	}
	frame(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("resp_%s_%d", identity, step), "status": "completed", "usage": map[string]any{"input_tokens": 100, "input_tokens_details": map[string]int{"cached_tokens": 32, "cache_write_tokens": 8}, "output_tokens": 10, "output_tokens_details": map[string]int{"reasoning_tokens": 2}, "total_tokens": 110}}})
}

func TestHTTPMockOpenAIIntegrationTwentyToolTypes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("LSP fixture requires python3")
	}
	r, _ := runtimeFixture(t, nil)
	root := r.Workspace.Root
	for name, text := range map[string]string{"sample.py": "a😀éz\nsecond\n", "README.md": "# Fixture\n\nA reproducible integration project.\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("../lsp/testdata/server.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.py"), script, 0600); err != nil {
		t.Fatal(err)
	}
	img := image.NewNRGBA(image.Rect(0, 0, 16, 10))
	img.Set(3, 4, color.NRGBA{R: 200, B: 240, A: 255})
	f, err := os.Create(filepath.Join(root, "fixture.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_EXA_URL", "http://mock.invalid/mcp")
	t.Setenv("TTC_LSP_MODE", "normal")
	t.Setenv("TTC_LSP_ENCODING", "utf-16")
	var mu sync.Mutex
	counts := map[string]int{}
	requests := 0
	failures := make(chan error, 64)
	var droppedFailures atomic.Int64
	failure := func(err error) {
		select {
		case failures <- err:
		default:
			droppedFailures.Add(1) // A failing fixture must not strand an HTTP handler.
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	client := mockHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/notes" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<h1>Notes</h1><p>fixture navigation</p><table><tr><th>Stage</th><th>Result</th></tr><tr><td>HTTP</td><td>offline</td></tr></table>")
			return
		}
		if request.URL.Path == "/mcp" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Fixture result http://mock.invalid/notes\nUseful navigation notes."}]}}`)
			return
		}
		if request.Header.Get("Authorization") != "Bearer mock-token" || request.Header.Get("ChatGPT-Account-ID") != "mock-account" {
			failure(errors.New("mock auth headers missing"))
			http.Error(w, "bad auth", 401)
			return
		}
		if request.URL.Path == "/models" {
			_, _ = io.WriteString(w, `{"models":[{"slug":"mock","display_name":"Mock OpenAI","visibility":"list","context_window":272000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}],"input_modalities":["text","image"]}]}`)
			return
		}
		if request.URL.Path != "/responses" {
			failure(fmt.Errorf("unexpected URL %s", request.URL))
			http.Error(w, "unknown", 400)
			return
		}
		var body struct {
			Instructions string `json:"instructions"`
			Store        *bool  `json:"store"`
			Input        []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			failure(err)
			http.Error(w, "bad JSON", 400)
			return
		}
		if body.Store == nil || *body.Store {
			failure(errors.New("store must be false"))
		}
		identity := "m"
		if body.Instructions == childSystemTemplate {
			identity = "c"
		} else if body.Instructions != systemTemplate {
			failure(errors.New("coding instructions changed"))
		}
		mu.Lock()
		step := counts[identity]
		counts[identity]++
		requests++
		mu.Unlock()
		results := map[string]map[string]any{}
		for _, item := range body.Input {
			if item.Type == "function_call_output" {
				var result map[string]any
				if err := json.Unmarshal([]byte(item.Output), &result); err != nil {
					failure(err)
				}
				results[item.CallID] = result
				if result["ok"] != true {
					failure(fmt.Errorf("tool %s failed: %s", item.CallID, item.Output))
				}
			}
		}
		get := func(id, key string) string {
			value, ok := results[id][key].(string)
			if !ok || value == "" {
				failure(fmt.Errorf("missing %s.%s in results", id, key))
			}
			return value
		}
		var calls []mockCall
		text := ""
		if identity == "c" {
			switch step {
			case 0:
				calls = []mockCall{{"write", map[string]any{"path": "child.txt", "content": "one\n"}}}
			case 1:
				text = "First child assignment complete."
			case 2:
				calls = []mockCall{{"edit", map[string]any{"path": "child.txt", "old_text": "one", "new_text": "two"}}}
			case 3:
				text = "Follow-up complete."
			default:
				failure(fmt.Errorf("unexpected child step %d", step))
				text = "Unexpected child step"
			}
		} else {
			switch step {
			case 0:
				calls = []mockCall{
					{"glob", map[string]any{"pattern": "*.py"}}, {"grep", map[string]any{"pattern": "Fixture", "path": "README.md"}}, {"read", map[string]any{"path": "README.md"}}, {"skill", map[string]any{"name": "lsp"}},
					{"web_fetch", map[string]any{"url": "http://mock.invalid/notes"}}, {"web_search", map[string]any{"query": "fixture navigation"}}, {"image_show", map[string]any{"path": "fixture.png", "request_click": false}},
					{"question", map[string]any{"questions": []any{map[string]any{"id": "format", "prompt": "Choose output", "recommended_option_id": "md", "options": []any{map[string]string{"id": "md", "label": "Markdown"}, map[string]string{"id": "txt", "label": "Text"}}}}}},
					{"shell", map[string]any{"command": "printf 'stdout fixture\\n'; printf 'stderr fixture\\n' >&2"}}, {"wakeup_schedule", map[string]any{"name": "fixture-check", "message": "Check fixture", "delay_seconds": 60}},
				}
			case 1:
				calls = []mockCall{
					{"write", map[string]any{"path": "result.txt", "content": "alpha\n"}}, {"shell", map[string]any{"command": "exec " + quote(python) + " server.py", "protocol": "lsp", "background": true, "wake_on_exit": false}}, {"subagent", map[string]any{"prompt": "Write the isolated fixture result", "label": "fixture child"}},
				}
			case 2:
				calls = []mockCall{
					{"lsp_query", map[string]any{"job_id": get("m1_shell", "job_id"), "operation": "definition", "path": "sample.py", "line": 1, "column": 3, "limit": 1}}, {"edit", map[string]any{"path": "result.txt", "old_text": "alpha", "new_text": "beta"}}, {"patch", map[string]any{"patch_text": "*** Begin Patch\n*** Update File: result.txt\n@@\n-beta\n+gamma\n*** End Patch"}},
					{"job_read", map[string]any{"job_id": get("m0_shell", "job_id"), "stream": "stderr", "cursor": "eof:-10:lines", "grep": "fixture"}}, {"job_list", map[string]any{"state": "all"}}, {"wakeup_list", map[string]any{}},
					{"web_fetch", map[string]any{"document_id": get("m0_web_fetch", "document_id"), "pattern": "fixture", "context_lines": 1}}, {"subagent", map[string]any{"child_id": get("m1_subagent", "child_id"), "prompt": "Follow up by updating the retained result"}},
				}
			case 3:
				calls = []mockCall{
					{"job_stop", map[string]any{"child_id": get("m2_subagent", "child_id")}}, {"wakeup_cancel", map[string]any{"wakeup_id": get("m0_wakeup_schedule", "wakeup_id")}},
					{"job_stop", map[string]any{"job_id": get("m1_shell", "job_id")}},
				}
			case 4:
				text = "# HTTP mock complete\n\n| Feature | Result |\n| --- | --- |\n| Tools | verified |\n\n- Shared child edits\n- Inline math $E=mc^2$\n\n$$\\int_0^1 x^2\\,dx = \\frac{1}{3}$$\n\n```python\nprint('offline')\n```\n\n> Deterministic fixture.\n\n[Notes](http://mock.invalid/notes) and `inline code`."
			default:
				failure(fmt.Errorf("unexpected main step %d", step))
				text = "Unexpected main step"
			}
		}
		streamMockResponse(w, identity, step, calls, text)
	}))
	path := filepath.Join(t.TempDir(), "auth.json")
	credentials, _ := json.Marshal(openai.Credentials{AuthMode: "chatgpt", Tokens: openai.Tokens{Access: "mock-token", AccountID: "mock-account"}, LastRefresh: time.Now()})
	if err := os.WriteFile(path, credentials, 0600); err != nil {
		t.Fatal(err)
	}
	adapter := openai.New(path)
	adapter.BaseURL = "http://mock.invalid"
	adapter.Client = client
	models, err := adapter.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	selection, err := provider.Resolve("openai", models, "mock", "low")
	if err != nil {
		t.Fatal(err)
	}
	r.Provider = adapter
	r.selection = selection
	r.Tools = r.Tools.Filter(func(name string) bool { return name != "web_fetch" && name != "web_search" })
	tool.AddWeb(r.Tools, client)
	events := make(chan Event, 1024)
	r.Emit = func(event Event) { events <- event }
	consumerDone := make(chan struct{})
	stopConsumer := make(chan struct{})
	var stopOnce sync.Once
	stopEvents := func() { stopOnce.Do(func() { close(stopConsumer) }); <-consumerDone }
	var announced, questions int
	go func() {
		defer close(consumerDone)
		for {
			select {
			case event := <-events:
				if strings.Contains(event.Text, "awaiting ") {
					announced++
				}
				if event.Kind == "question" {
					questions++
					answers := []Answer{{ID: "format", Values: []string{"md"}, Source: "option"}}
					if err := r.AnswerQuestion(event.Question.ID, answers); err != nil {
						failure(err)
					}
				}
			case <-stopConsumer:
				return
			}
		}
	}()
	t.Cleanup(func() { r.Close(); stopEvents() })
	if err := r.Run(&provider.Message{Role: "user", Content: "Exercise the reproducible HTTP fixture"}); err != nil {
		t.Fatal(err)
	}
	// End-to-end persistence and shared file queue assertions.
	for path, want := range map[string]string{"result.txt": "gamma\n", "child.txt": "two\n"} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != want {
			t.Fatal(path, string(got), err)
		}
	}
	var names []string
	rows, err := r.Store.DB.Query("SELECT DISTINCT name FROM tool_calls ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 20 {
		t.Fatal("tool type coverage", len(names), names)
	}
	var broken int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM tool_calls WHERE result_json IS NULL OR json_extract(result_json,'$.ok')!=1").Scan(&broken); err != nil || broken != 0 {
		t.Fatal("tool failure", broken, err)
	}
	if len(r.ChildViews("main")) != 0 {
		t.Fatal("closed child retained")
	}
	if len(r.Jobs.Live()) != 0 {
		t.Fatal("integration left jobs running", r.Jobs.Live())
	}
	var owners int
	if err := r.Store.DB.QueryRow("SELECT count(DISTINCT undo_owner_turn_id) FROM entries WHERE json_extract(content_json,'$.type')='file_changed'").Scan(&owners); err != nil || owners != 1 {
		t.Fatal("child edits diverged from main undo ownership", owners, err)
	}
	var finishes, acknowledged, requestsStored int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM entries WHERE json_extract(content_json,'$.type')='child_turn_finished'").Scan(&finishes); err != nil || finishes != 2 {
		t.Fatal(finishes, err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(DISTINCT value) FROM model_requests,json_each(delivered_events_json)").Scan(&acknowledged); err != nil || acknowledged < 2 {
		t.Fatal("child completion acknowledgments", acknowledged, err)
	}
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM model_requests WHERE json_extract(input_json,'$.version')=1 AND json_extract(input_json,'$.message_count')>0 AND event_cutoff>0").Scan(&requestsStored); err != nil || requestsStored != 9 {
		t.Fatal(requestsStored, err)
	}
	usage := r.UsageSnapshot().Totals
	if usage.Requests != 9 || usage.ReportedRequests != 9 || usage.Tokens.InputTokens != 900 || usage.Tokens.OutputTokens != 90 || usage.Tokens.CachedInputTokens == nil || *usage.Tokens.CachedInputTokens != 288 {
		t.Fatal("all-agent usage", usage)
	}
	archive, err := r.Store.ArchiveTranscript(r.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{archive, archive + ".jsonl"} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatal("archive missing", path, err)
		}
	}
	export := filepath.Join(t.TempDir(), "conversation.md")
	if err := r.Store.Export(r.Current(), export); err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(export)
	if err != nil || !strings.Contains(string(markdown), "# HTTP mock complete") || !strings.Contains(string(markdown), "| Feature | Result |") || strings.Contains(string(markdown), systemTemplate) {
		t.Fatal("Markdown export lost presentation or leaked instruction body", err)
	}
	exact, err := os.ReadFile(export + ".jsonl")
	if err != nil || !strings.Contains(string(exact), `"event_seq"`) || !strings.Contains(string(exact), `"instructions"`) || !strings.Contains(string(exact), `"tool_record"`) {
		t.Fatal("exact export lost provenance", err)
	}
	if _, err := r.Command("/undo"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"result.txt", "child.txt"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatal("main undo did not include child edits", path, err)
		}
	}
	if _, err := r.Command("/redo"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"result.txt": "gamma\n", "child.txt": "two\n"} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != want {
			t.Fatal("redo lost shared edits", path, string(got), err)
		}
	}
	stopEvents()
	if questions != 1 || announced == 0 {
		t.Fatal("typed question or pending-arguments UI event missing", questions, announced)
	}
	if len(failures) > 0 {
		for len(failures) > 0 {
			t.Error(<-failures)
		}
	}
	if dropped := droppedFailures.Load(); dropped > 0 {
		t.Errorf("mock failure buffer dropped %d additional errors", dropped)
	}
	sort.Strings(names)
	t.Logf("HTTP requests=%d, tools=%d %v; cumulative usage input=%d cached=%d output=%d; archive=%s", requests, len(names), names, usage.Tokens.InputTokens, *usage.Tokens.CachedInputTokens, usage.Tokens.OutputTokens, archive)
}
