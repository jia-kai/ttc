package openai

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"ttc/internal/llm"
)

// Guard inner error constructors too: every authored Stream failure can appear
// in ChildFinish.Error, not only the outer attempt/partial wrappers.
func TestStreamDiagnosticsUsePromptAssets(t *testing.T) {
	for _, name := range []string{"adapter.go", "stream.go", "state.go", "diagnostics.go", "retry.go", "auth.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && name == "auth.go" && (fn.Name.Name == "Login" || fn.Name.Name == "ImportCodex") {
				return false // UI-only entry points; their shared refresh helpers are checked.
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := fn.X.(*ast.Ident)
			if !ok || !(pkg.Name == "errors" && fn.Sel.Name == "New" || pkg.Name == "fmt" && (fn.Sel.Name == "Errorf" || fn.Sel.Name == "Sprintf")) {
				return true
			}
			ast.Inspect(call.Args[0], func(arg ast.Node) bool {
				if literal, ok := arg.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					t.Errorf("%s: authored diagnostic literal must live in prompt/provider-messages.yaml", fset.Position(literal.Pos()))
				}
				return true
			})
			return true
		})
	}
}

func TestProviderDiagnosticExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"missing response", httpFailure("", nil), "subscription response: missing HTTP response"},
		{"HTTP absent", httpFailure("", &http.Response{StatusCode: 503}), "subscription response HTTP 503: error details missing; body absent"},
		{"HTTP read failure", httpFailure("", &http.Response{StatusCode: 502, Body: io.NopCloser(&failureTestReader{err: errors.New("private")})}), "subscription response HTTP 502: body read failed; error details missing; body absent"},
		{"HTTP truncation", httpFailure("", &http.Response{StatusCode: 502, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", failureBodyLimit+1)))}), "subscription response HTTP 502: body truncated; error details missing; body non-JSON or invalid JSON"},
		{"stream shape", streamFailure("", []byte(`{"type":"error"}`), "error"), `subscription stream terminated: error; error details missing; shape=object{"type":string}`},
		{"stream scalar", streamFailure("", []byte(`"private"`), "error"), "subscription stream terminated: error; error details missing; shape=string"},
		{"stream inspection limit", streamFailure("", []byte(strings.Repeat("x", failureStreamLimit+1)), "error"), "subscription stream terminated: error; error details omitted; event exceeds diagnostic inspection limit (64 KiB)"},
		{"stream redaction", failureWithRequestID(streamFailure("secret", []byte(`{"type":"error","message":"secret\nmessage","param":"input"}`), "error"), "request\nID"), "subscription stream terminated: error; message=[redacted] message; parameter=input; request_id=request ID"},
		{"transport DNS", transportFailure(&net.DNSError{Err: "private", Name: "private"}), "subscription transport failed before response: type=*net.DNSError DNS resolution failed"},
		{"transport timeout", transportFailure(&net.OpError{Err: context.DeadlineExceeded}), "subscription transport failed before response: type=*net.OpError timeout"},
		{"transport fallback", transportFailure(errors.New("private")), "subscription transport failed before response: type=*errors.errorString"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil || tc.err.Error() != tc.want {
				t.Fatalf("diagnostic = %q, want %q", tc.err, tc.want)
			}
		})
	}
	if got := safeFailureText("界界界", 7); got != "界…" {
		t.Fatalf("UTF-8 truncation = %q, want %q", got, "界…")
	}
}

func TestAuthRefreshStreamDiagnosticExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
		transport        error
		transient        bool
	}{
		{"OAuth error", `{"error":"invalid_grant","error_description":"private-refresh-fixture\nDenied"}`, "upstream attempt 1/1 failed: authentication response HTTP 401: code=invalid_grant; message=[redacted] Denied; request_id=refresh ID", 401, nil, true},
		{"missing body", "", "upstream attempt 1/1 failed: authentication response HTTP 503: error details missing; body absent; request_id=refresh ID", 503, nil, true},
		{"non-JSON body", "private body", "upstream attempt 1/1 failed: authentication response HTTP 503: error details missing; body non-JSON or invalid JSON; request_id=refresh ID", 503, nil, true},
		{"truncated body", strings.Repeat("x", failureBodyLimit+1), "upstream attempt 1/1 failed: authentication response HTTP 503: error details missing; body truncated; request_id=refresh ID", 503, nil, true},
		{"malformed success", `{"access_token":123}`, "upstream attempt 1/1 failed: invalid authentication response (HTTP 200); request_id=refresh ID", 200, nil, true},
		{"missing token", `{}`, "upstream attempt 1/1 failed: authentication refresh returned no access token (HTTP 200); request_id=refresh ID", 200, nil, true},
		{"transport", "", "upstream attempt 1/1 failed: authentication transport failed", 0, errors.New("private transport payload"), true},
		{"certificate", "", "authentication TLS certificate verification failed", 0, x509.UnknownAuthorityError{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := expiredUpstreamAuth(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("x-request-id", "refresh ID")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			if tc.transport != nil {
				a.Client.Transport = retryTransport(func(*http.Request) (*http.Response, error) { return nil, tc.transport })
			}
			err := transportAdapter(a).Stream(context.Background(), partialRequest(0, 1), func(ev llm.StreamEvent) error {
				t.Errorf("single-attempt refresh failure emitted %s", ev.Kind)
				return nil
			})
			var transient *llm.TransientError
			if err == nil || err.Error() != tc.want || errors.As(err, &transient) != tc.transient {
				t.Fatalf("error = %q (transient=%t), want %q (transient=%t)", err, transient != nil, tc.want, tc.transient)
			}
		})
	}
}

func TestWireInnerDiagnosticExactBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []llm.Message
		want     string
	}{
		{"invalid canonical call", []llm.Message{{Role: "assistant", Calls: []llm.ToolCall{{Arguments: []byte(`invalid`)}}}}, "invalid call JSON in history"},
		{"unsupported replay", []llm.Message{{Role: "assistant", State: &llm.ReplayState{Provider: "openai", Model: "scripted", Version: 99}}}, "unsupported OpenAI replay state"},
		{"invalid replay fields", []llm.Message{{Role: "assistant", State: &llm.ReplayState{Provider: "openai", Model: "scripted", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"type":123}`)}}}}, "invalid OpenAI replay item fields"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := partialRequest(0, 1)
			request.Messages = tc.messages
			_, err := wire(context.Background(), request, nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
		})
	}
}
