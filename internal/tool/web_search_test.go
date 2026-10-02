package tool

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

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
