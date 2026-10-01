package tool

import (
	"encoding/json"
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
			_, err := searchResult(strings.NewReader(body), sse)
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
