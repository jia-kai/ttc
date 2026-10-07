package openai

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"ttc/internal/llm"
)

func TestUpstreamStreamFailureDetails(t *testing.T) {
	for _, tc := range []struct {
		name, kind, raw string
		want            []string
	}{
		{"flat", "error", `{"type":"error","code":"invalid_request_error","message":"Reduce input.","param":"input"}`, []string{"error", "code=invalid_request_error", "message=Reduce input.", "parameter=input"}},
		{"subscription nested", "error", `{"type":"error","error":{"type":"error","code":"usage_limit_reached","message":"Try later.","param":"model","plan_type":"PRIVATE_PLAN"}}`, []string{"type=error", "code=usage_limit_reached", "message=Try later.", "parameter=model"}},
		{"failed", "response.failed", `{"type":"response.failed","response":{"id":"resp_fixture","error":{"type":"server_error","code":"server_error","message":"Try again.","param":"tools"},"output":[{"text":"PRIVATE_OUTPUT"}]}}`, []string{"response.failed", "response_id=resp_fixture", "type=server_error", "code=server_error", "message=Try again.", "parameter=tools"}},
		{"incomplete", "response.incomplete", `{"type":"response.incomplete","response":{"id":"resp_incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, []string{"response.incomplete", "response_id=resp_incomplete", "reason=max_output_tokens"}},
		{"response id field", "error", `{"type":"error","response_id":"resp_top","message":"Try again."}`, []string{"response_id=resp_top", "message=Try again."}},
		{"null fields", "error", `{"type":"error","code":null,"message":"Try again.","param":null}`, []string{"message=Try again."}},
		{"bare error", "error", `{"type":"error"}`, []string{"error details missing", `"type":string`}},
		{"unreadable details", "error", `{"type":"error","message":"\u202e\n\t"}`, []string{"error details missing", `"message":string`}},
		{"empty error", "error", `{"type":"error","error":{}}`, []string{"error details missing", `"error":object{}`}},
		{"missing response error", "response.failed", `{"type":"response.failed","response":{"id":"resp_missing","error":null}}`, []string{"response_id=resp_missing", "error details missing", `"error":null`}},
		{"missing incomplete reason", "response.incomplete", `{"type":"response.incomplete","response":{"incomplete_details":{}}}`, []string{"error details missing", `"incomplete_details":object`}},
		{"wrong field types", "error", `{"type":"error","error":{"code":{"secret":"PRIVATE_CODE"},"message":["PRIVATE_MESSAGE"],"param":true}}`, []string{"error details missing", `"code":object`, `"message":array`, `"param":boolean`}},
		{"partly readable", "error", `{"type":"error","error":{"type":"server_error","code":{"secret":"PRIVATE_CODE"},"message":["PRIVATE_MESSAGE"]}}`, []string{"type=server_error", `"code":object`, `"message":array`}},
		{"unfamiliar", "error", `{"type":"error","mystery":{"detail":"PRIVATE_DETAIL","retry":false},"output":["PRIVATE_OUTPUT"],"encrypted_content":"PRIVATE_REASONING","headers":{"Authorization":"PRIVATE_TOKEN"},"credentials":"PRIVATE_PASSWORD"}`, []string{"error details missing", `"mystery":object`, `"detail":string`, `"retry":boolean`, `"output":array`, `"encrypted_content":string`}},
		{"scalar", "error", `"PRIVATE_SCALAR"`, []string{"error details missing", "shape=string"}},
		{"array", "error", `["PRIVATE_ARRAY"]`, []string{"error details missing", "shape=array"}},
		{"invalid JSON", "error", `PRIVATE_NOT_JSON`, []string{"error details missing", "non-JSON or invalid JSON"}},
		{"absent", "error", " \n\t", []string{"error details missing", "body absent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := streamFailure("", []byte(tc.raw), tc.kind)
			assertFailureContains(t, err, tc.want...)
			assertSafeFailure(t, err.Error(), "subscription stream terminated: ")
			if strings.Contains(err.Error(), "PRIVATE_") {
				t.Fatalf("unallowlisted value leaked: %v", err)
			}
			var transient *llm.TransientError
			if errors.As(err, &transient) {
				t.Fatalf("diagnostic assigned retry policy: %v", err)
			}
		})
	}
}

func TestUpstreamDiagnosticsSanitizeAndBoundUnicode(t *testing.T) {
	controls := "\x00\x1b\n\t\r\u0085\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u2028\u2029"
	payload, err := json.Marshal(map[string]any{
		"type": "error", "response_id": "resp" + controls + "safe", "error": map[string]string{
			"type": "server_error" + controls, "code": "server_error", "message": "研究 🚀" + controls + strings.Repeat("界🚀", 3000), "param": "tools" + controls + "safe",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := failureWithRequestID(streamFailure("", payload, "error"+controls), "req"+controls+strings.Repeat("研究", 1000))
	assertFailureContains(t, failure, "研究 🚀", "parameter=tools safe", "response_id=resp safe", "request_id=req")
	assertSafeFailure(t, failure.Error(), "subscription stream terminated: ")
	if !strings.Contains(failure.Error(), "…") {
		t.Fatal("long fields were not marked truncated", failure)
	}

	// Invalid UTF-8 strings in otherwise valid JSON are replaced, never emitted.
	failure = streamFailure("", []byte("{\"type\":\"error\",\"message\":\"bad\xffbyte\"}"), "error")
	assertSafeFailure(t, failure.Error(), "subscription stream terminated: ")
	assertFailureContains(t, failure, "bad�byte")
}

func TestUpstreamDiagnosticsBoundUnknownStructure(t *testing.T) {
	fields := make(map[string]any)
	for i := range 100 {
		fields[strings.Repeat("研究", 100)+string(rune('A'+i))] = map[string]any{"private": "PRIVATE_VALUE"}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	failure := streamFailure("", payload, "error")
	assertSafeFailure(t, failure.Error(), "subscription stream terminated: ")
	assertFailureContains(t, failure, "error details missing", "shape=object", "…")
	if strings.Contains(failure.Error(), "PRIVATE_VALUE") {
		t.Fatal("structural diagnostic emitted value", failure)
	}
}

func TestUpstreamHTTPFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"nested", `{"error":{"type":"invalid_request_error","code":"unsupported_model","message":"Choose another model.","param":"model","credentials":"PRIVATE_CREDENTIAL"},"output":"PRIVATE_OUTPUT"}`, []string{"type=invalid_request_error", "code=unsupported_model", "message=Choose another model.", "parameter=model"}},
		{"flat", `{"type":"invalid_request_error","code":"bad_input","message":"Reduce input.","param":"input","headers":{"Authorization":"PRIVATE_TOKEN"}}`, []string{"type=invalid_request_error", "code=bad_input", "message=Reduce input.", "parameter=input"}},
		{"empty", "", []string{"body absent", "error details missing"}},
		{"HTML", "<html>PRIVATE_PROXY_BODY</html>", []string{"non-JSON or invalid JSON"}},
		{"wrong shape", `{"surprise":{"token":"PRIVATE_TOKEN"}}`, []string{"error details missing", `"surprise":object`, `"token":string`}},
		{"truncated", strings.Repeat("PRIVATE_BODY", 1000), []string{"body truncated", "non-JSON or invalid JSON"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 403, Status: "PRIVATE_STATUS", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}
			resp.Header.Set("x-request-id", "req_fixture\n\x1b\u202e safe")
			resp.Header.Set("Authorization", "PRIVATE_HEADER")
			failure := httpFailure("", resp)
			assertFailureContains(t, failure, append(tc.want, "HTTP 403", "request_id=req_fixture safe")...)
			assertSafeFailure(t, failure.Error(), "subscription response HTTP 403: ")
			if strings.Contains(failure.Error(), "PRIVATE_") {
				t.Fatal("private response value leaked", failure)
			}
		})
	}
}

func TestUpstreamHTTPFailureBoundsReadAndHidesReaderError(t *testing.T) {
	reader := &failureTestReader{remaining: 100000}
	failure := httpFailure("", &http.Response{StatusCode: 502, Body: io.NopCloser(reader)})
	if reader.read != failureBodyLimit+1 {
		t.Fatalf("read %d bytes, want %d", reader.read, failureBodyLimit+1)
	}
	assertFailureContains(t, failure, "HTTP 502", "body truncated")

	reader = &failureTestReader{remaining: 0, err: errors.New("PRIVATE_READER_ERROR")}
	failure = httpFailure("", &http.Response{StatusCode: 502, Body: io.NopCloser(reader)})
	assertFailureContains(t, failure, "body read failed", "body absent")
	if strings.Contains(failure.Error(), "PRIVATE_") {
		t.Fatal("reader error leaked", failure)
	}
	assertFailureContains(t, httpFailure("", &http.Response{StatusCode: 401}), "HTTP 401", "body absent")
	assertFailureContains(t, httpFailure("", nil), "missing HTTP response")
}

func TestFailureWithRequestIDPreservesCause(t *testing.T) {
	cause := &failureTestError{}
	wrapped := failureWithRequestID(cause, "request\u202e\x1b\n"+strings.Repeat("🚀", 1000))
	var typed *failureTestError
	if !errors.Is(wrapped, cause) || !errors.As(wrapped, &typed) || typed != cause {
		t.Fatal("request ID wrapper lost error identity", wrapped)
	}
	assertFailureContains(t, wrapped, "request_id=request", "…")
	assertSafeFailure(t, wrapped.Error(), "")
	if failureWithRequestID(cause, " \n\u202e") != cause || failureWithRequestID(nil, "id") != nil {
		t.Fatal("empty ID or nil error changed semantics")
	}
}

type failureTestError struct{}

func (*failureTestError) Error() string { return "upstream failed" }

type failureTestReader struct {
	remaining, read int
	err             error
}

func (r *failureTestReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}

func assertFailureContains(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("missing diagnostic")
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("diagnostic missing %q: %v", fragment, err)
		}
	}
}

func assertSafeFailure(t *testing.T, text, prefix string) {
	t.Helper()
	if len(text) > failureDiagnosticLimit+len(prefix) || !utf8.ValidString(text) {
		t.Fatalf("invalid or unbounded diagnostic (%d bytes): %q", len(text), text)
	}
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			t.Fatalf("unsafe diagnostic rune %U: %q", r, text)
		}
	}
}
