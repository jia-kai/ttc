package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCheckWebSearchResponses(t *testing.T) {
	const success = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Exa source"}]}}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"success", 200, success, ""},
		{"unauthorized", 401, "private diagnostic", "401"},
		{"forbidden", 403, "private diagnostic", "403"},
		{"rate limited", 429, "private diagnostic", "rate"},
		{"malformed", 200, "not JSON", "decode"},
		{"rpc error", 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"retry later"}}`, "-32602"},
		{"tool error", 200, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"retry later"}]}}`, "retry later"},
		{"missing result", 200, `{"jsonrpc":"2.0","id":1}`, "result"},
		{"wrong id", 200, `{"jsonrpc":"2.0","id":2,"result":{}}`, "id"},
		{"wrong version", 200, `{"jsonrpc":"1.0","id":1,"result":{}}`, "version"},
	} {
		for _, sse := range []bool{false, true} {
			t.Run(tc.name+fmtBool(sse), func(t *testing.T) {
				calls := 0
				client := &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.Method != "POST" || req.URL.String() != "https://mcp.exa.ai/mcp" || req.Header.Get("x-api-key") != "" || req.Header.Get("Content-Type") != "application/json" || req.Header.Get("Accept") != "application/json, text/event-stream" {
						t.Fatalf("unexpected request: %v %v", req, req.Header)
					}
					deadline, ok := req.Context().Deadline()
					if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 30*time.Second {
						t.Fatalf("missing 30-second bound: %v", deadline)
					}
					var payload struct {
						Version string `json:"jsonrpc"`
						ID      int    `json:"id"`
						Method  string `json:"method"`
						Params  struct {
							Name      string `json:"name"`
							Arguments struct {
								Query      string `json:"query"`
								NumResults int    `json:"numResults"`
							} `json:"arguments"`
						} `json:"params"`
					}
					if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
						t.Fatal(err)
					}
					if payload.Version != "2.0" || payload.ID != 1 || payload.Method != "tools/call" || payload.Params.Name != "web_search_exa" || payload.Params.Arguments.Query != "Exa" || payload.Params.Arguments.NumResults != 1 {
						t.Fatalf("not a minimal real search: %+v", payload)
					}
					body := tc.body
					header := http.Header{"Content-Type": {"application/json"}}
					if sse {
						body = "event: message\ndata: " + body + "\n\n"
						header.Set("Content-Type", "text/event-stream")
					}
					return &http.Response{StatusCode: tc.status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				})}
				err := CheckWebSearch(context.Background(), client, WebSearchConfig{})
				want := tc.want
				if sse && tc.name == "wrong id" {
					want = "no matching result"
				}
				if calls != 1 || (want == "" && err != nil) || (want != "" && (err == nil || !strings.Contains(strings.ToLower(err.Error()), want))) {
					t.Fatalf("calls=%d error=%v; want %q", calls, err, want)
				}
				if tc.status == 429 {
					var structured *Error
					if !errors.As(err, &structured) || structured.Code != "rate_limited" {
						t.Fatalf("lost rate-limit classification: %v", err)
					}
				}
			})
		}
	}
}

func fmtBool(sse bool) string {
	if sse {
		return "/SSE"
	}
	return "/JSON"
}

func TestCheckWebSearchSecretsAndRedirects(t *testing.T) {
	const key = "synthetic-private-key"
	const endpoint = "https://example.test/mcp?key=" + key
	for _, failure := range []string{"transport", "body", "rpc", "tool", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				t.Fatal("caller redirect policy must not run")
				return nil
			}, Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("x-api-key") != key {
					t.Fatal("missing credential")
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Request: req}
				switch failure {
				case "transport":
					return nil, errors.New("failed " + key + " " + endpoint)
				case "body":
					resp.Body = failedSearchBody{}
				case "rpc", "tool":
					message, _ := json.Marshal("rejected " + key + " " + endpoint)
					body := `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":` + string(message) + `}}`
					if failure == "tool" {
						body = `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":` + string(message) + `}]}}`
					}
					resp.Body = io.NopCloser(strings.NewReader(body))
				case "redirect":
					resp.StatusCode = 302
					resp.Header.Set("Location", endpoint+"&destination="+key)
					resp.Body = io.NopCloser(strings.NewReader(""))
				}
				return resp, nil
			})}
			err := CheckWebSearch(context.Background(), client, WebSearchConfig{Endpoint: endpoint, APIKey: key})
			if err == nil || calls != 1 || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "example.test/mcp") {
				t.Fatalf("unsafe error or extra request: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCheckWebSearchConfigAndCancellation(t *testing.T) {
	client := &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckWebSearch(ctx, client, WebSearchConfig{}); err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("lost caller cancellation diagnostic: %v", err)
	}
	client.Transport = fetchTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid config reached transport")
		return nil, nil
	})
	for _, config := range []WebSearchConfig{
		{Endpoint: "file:///private-key"},
		{Endpoint: "https://user:private-key@example.test/mcp"},
		{APIKey: "private-key\n"},
	} {
		if err := CheckWebSearch(context.Background(), client, config); err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatalf("invalid config not safely rejected: %v", err)
		}
	}
}

func TestWebSearchPreservesBoundedRecoveryDiagnostics(t *testing.T) {
	guidance := "Reduce numResults to 3, then retry."
	for _, rpc := range []bool{false, true} {
		var envelope map[string]any
		message := guidance + "\n\t\x1b\u202e" + strings.Repeat("研究", 3000)
		if rpc {
			envelope = map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32602, "message": message}}
		} else {
			envelope = map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"isError": true, "content": []any{
				map[string]string{"type": "image", "text": "ignored image data"},
				map[string]string{"type": "text", "text": message},
			}}}
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		for _, sse := range []bool{false, true} {
			body := string(encoded)
			if sse {
				body = "data: " + body + "\n\n"
			}
			_, err := searchResult(strings.NewReader(body), sse, "")
			if err == nil {
				t.Fatal("backend failure accepted")
			}
			text := err.Error()
			if !strings.Contains(text, guidance) || !utf8.ValidString(text) || len(text) > 4096+64 || !strings.HasSuffix(text, "…") || strings.ContainsAny(text, "\x1b\u202e\n\t") || strings.Contains(text, "ignored image data") {
				t.Fatalf("recovery error must be useful, bounded and control-safe: %q", text)
			}
			if rpc && !strings.Contains(text, "-32602") {
				t.Fatalf("lost backend error code: %q", text)
			}
		}
	}
}

func TestSearchCredentialsNeverEnterModelResults(t *testing.T) {
	const key = "synthetic-private-key"
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"rejected synthetic-private-key"}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"rejected synthetic-private-key"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"found synthetic-private-key"}]}}`,
	} {
		for _, sse := range []bool{false, true} {
			text := body
			if sse {
				text = "data: " + body + "\n\n"
			}
			result, err := searchResult(strings.NewReader(text), sse, key)
			if strings.Contains(result, key) || (err != nil && strings.Contains(err.Error(), key)) {
				t.Fatal("credential exposed")
			}
			if err == nil && !strings.Contains(result, "[redacted]") {
				t.Fatal("result not redacted")
			}
		}
	}
}

// failedSearchBody simulates a read failure whose diagnostic echoes a credential.
type failedSearchBody struct{}

func (failedSearchBody) Read([]byte) (int, error) {
	return 0, errors.New("body read synthetic-private-key")
}
func (failedSearchBody) Close() error { return nil }

func TestSearchTransportConfigStaysOutOfRecords(t *testing.T) {
	const key = "synthetic-private-key"
	for _, failure := range []string{"", "transport", "body"} {
		t.Run(failure, func(t *testing.T) {
			r, w, x, request := toolFixture(t)
			AddWeb(r, &http.Client{Transport: fetchTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("x-api-key") != key {
					t.Fatal("missing operator credential")
				}
				if failure == "transport" {
					return nil, errors.New("failed synthetic-private-key")
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Useful source"}]}}`))
				if failure == "body" {
					body = failedSearchBody{}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: body, Request: req}, nil
			})}, WebSearchConfig{Endpoint: "https://example.test/mcp?key=" + key, APIKey: key})
			record := invoke(t, r, w, x, request, "web_search", `{"query":"test"}`)
			encoded, err := json.Marshal(record)
			if err != nil || strings.Contains(string(encoded), key) || strings.Contains(string(encoded), "example.test/mcp") {
				t.Fatal("transport settings exposed", err)
			}
			if failure == "" {
				ok(t, record)
			} else if !strings.Contains(string(record.Result), "[redacted]") {
				t.Fatal("unredacted failure")
			}
		})
	}
}
