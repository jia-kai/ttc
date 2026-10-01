package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWebMarkdownSemantics(t *testing.T) {
	base, _ := url.Parse("https://example.test/docs/start")
	source := `<html><head><title>noise</title></head><body><nav>ignore</nav><main><h1>Title</h1><p>Before <strong>bold</strong> after <a href="../guide">guide</a>.</p><ol><li>one</li><li>two</li></ol><pre><code class="language-go">x := 1
</code></pre><p>Use <code>x</code>.</p><table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table><script>bad</script></main></body></html>`
	got, err := htmlMarkdown(context.Background(), source, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Title", "Before **bold** after", "[guide](https://example.test/guide)", "1. one", "2. two", "```go", "x := 1", "| A | B |", "| 1 | 2 |"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	for _, bad := range []string{"noise", "ignore", "bad"} {
		if strings.Contains(got, bad) {
			t.Fatal(got)
		}
	}
}

func TestWebMarkdownBoundsSparseTablePadding(t *testing.T) {
	base, _ := url.Parse("https://example.test/table")
	// The encoded page is small, but rectangular Markdown padding would be
	// over 4 MiB. Reject it before materializing those missing cells.
	source := "<table><tr>" + strings.Repeat("<th>x</th>", 1500) + "</tr>" + strings.Repeat("<tr><td>x</td></tr>", 1500) + "</table>"
	_, err := htmlMarkdown(context.Background(), source, base)
	if err == nil || !strings.Contains(err.Error(), "format=text") {
		t.Fatal(err)
	}
}

func TestWebFetchRetainedPagingAndSearch(t *testing.T) {
	r, w, x, request := toolFixture(t)
	downloads := 0
	AddWeb(r, &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: io.NopCloser(strings.NewReader("αβγ\nmatch first\nother\nmatch second\n")), Request: req}, nil
	})})
	call := func(args string) map[string]any {
		t.Helper()
		record := invoke(t, r, w, x, request, "web_fetch", args)
		ok(t, record)
		var v map[string]any
		if err := json.Unmarshal(record.Result, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := call(`{"url":"https://example.test/page","max_chars":2}`)
	if first["content"] != "αβ" || first["next_offset"] != float64(2) {
		t.Fatal(first)
	}
	id := first["document_id"].(string)
	args, _ := json.Marshal(map[string]any{"document_id": id, "offset": 2, "max_chars": 2})
	second := call(string(args))
	if second["content"] != "γ\n" || downloads != 1 {
		t.Fatal(second, downloads)
	}
	args, _ = json.Marshal(map[string]any{"document_id": id, "pattern": "^match", "max_chars": 12})
	matched := call(string(args))
	if len(matched["matches"].([]any)) != 1 || matched["next_offset"] != float64(1) {
		t.Fatal(matched)
	}
	args, _ = json.Marshal(map[string]any{"document_id": id, "pattern": "^match", "offset": 1})
	next := call(string(args))
	if !strings.Contains(next["matches"].([]any)[0].(map[string]any)["content"].(string), "second") {
		t.Fatal(next)
	}
	for _, args := range []string{`{}`, `{"url":"file:///tmp/a"}`, `{"url":"https://u:p@example.test"}`, `{"document_id":"missing"}`, `{"url":"https://example.test","pattern":"["}`, `{"url":"https://example.test","max_chars":0}`} {
		record := invoke(t, r, w, x, request, "web_fetch", args)
		var v struct{ OK bool }
		json.Unmarshal(record.Result, &v)
		if v.OK {
			t.Fatal("invalid input accepted", args)
		}
	}
}

func TestWebCacheBoundsExpiryAndImmutability(t *testing.T) {
	c := &webCache{}
	first, err := c.add(webDocument{text: "original"})
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := c.add(webDocument{text: "new"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.get(first.id); err == nil {
		t.Fatal("old document not evicted")
	}
	last := c.docs[len(c.docs)-1]
	last.text = "changed"
	stored, _ := c.get(last.id)
	if stored.text != "new" {
		t.Fatal("copy mutated cache")
	}
	c.docs[0].expires = time.Now().Add(-time.Second)
	expired := c.docs[0].id
	if _, err := c.get(expired); err == nil {
		t.Fatal("expired page returned")
	}
	if _, err := c.add(webDocument{text: strings.Repeat("x", webCacheBytes+1)}); err == nil {
		t.Fatal("oversize cache accepted")
	}
	a, _ := c.add(webDocument{text: strings.Repeat("x", 10<<20)})
	b, _ := c.add(webDocument{text: strings.Repeat("y", 10<<20)})
	if c.bytes > webCacheBytes {
		t.Fatal(c.bytes)
	}
	if _, err := c.get(a.id); err == nil {
		t.Fatal("cache pool not enforced")
	}
	if _, err := c.get(b.id); err != nil {
		t.Fatal(err)
	}
}

func TestWebSearchJSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[sse], func(t *testing.T) {
			t.Setenv("SCICODE_EXA_URL", "https://mock.test/mcp")
			t.Setenv("EXA_API_KEY", "synthetic-key")
			r, w, x, request := toolFixture(t)
			AddWeb(r, &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != "POST" || req.Header.Get("x-api-key") != "synthetic-key" {
					t.Fatal("wrong search request")
				}
				var body struct {
					Method string
					Params struct {
						Name      string
						Arguments map[string]any
					}
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Method != "tools/call" || body.Params.Name != "web_search_exa" || body.Params.Arguments["query"] != "a question" || body.Params.Arguments["numResults"] != float64(5) {
					t.Fatal(body)
				}
				data := `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Title: Source\nURL: https://example.test/\nHighlights: useful"}]}}`
				ct := "application/json"
				if sse {
					data = "event: message\ndata: " + data + "\n\n"
					ct = "text/event-stream"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(data)), Request: req}, nil
			})})
			rec := invoke(t, r, w, x, request, "web_search", `{"query":"a question","max_chars":20}`)
			ok(t, rec)
			var out struct {
				Content    string
				DocumentID string `json:"document_id"`
				Truncated  bool
			}
			json.Unmarshal(rec.Result, &out)
			if out.Content != "Title: Source\nURL: h" || !out.Truncated {
				t.Fatal(string(rec.Result))
			}
			// The same document ID can be searched through web_fetch without another POST.
			b, _ := json.Marshal(map[string]any{"document_id": out.DocumentID, "pattern": "Highlights"})
			result := invoke(t, r, w, x, request, "web_fetch", string(b))
			ok(t, result)
		})
	}
	for _, raw := range []string{`{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`, `{"jsonrpc":"2.0","id":1,"result":{"isError":true}}`, `{"jsonrpc":"2.0","id":2,"result":{}}`, `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, `not json`} {
		if _, err := searchResult(strings.NewReader(raw), false); err == nil {
			t.Fatal("accepted invalid response", raw)
		}
	}
}

func TestWebPageCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := webPage(ctx, webDocument{text: "match"}, webArgs{Pattern: "match"}); err == nil {
		t.Fatal("ignored cancellation")
	}
}
