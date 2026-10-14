package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/prompts"
)

func TestGuidanceTemplatesPreserveFormatting(t *testing.T) {
	for _, test := range []struct {
		name, got, want string
	}{
		{"operation", fmt.Sprintf(prompts.LSPUnsupportedOperation, "hover"), "language server does not advertise hover; choose another operation or server"},
		{"line", fmt.Sprintf(prompts.LSPLineBeyondFile, 7), "line 7 is beyond the file; read it and choose an existing line"},
		{"column", fmt.Sprintf(prompts.LSPColumnBeyondLine, 8, 2, 5), "column 8 exceeds line 2's end column 5; use Unicode code-point columns, not bytes"},
		{"framing", fmt.Sprintf(prompts.LSPInvalidFraming, "bad %s header"), "invalid LSP framing; with available lifecycle tools, restart using exec SERVER and emit no ordinary stdout; otherwise use another accessible server or report the configuration issue: bad %s header"},
		{"server error", fmt.Sprintf(prompts.LSPServerError, "textDocument/hover", -32602, "bad %s position"), "LSP textDocument/hover error -32602: bad %s position; check path/position and server configuration, then retry"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("got %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestGuidancePreservesErrorTypes(t *testing.T) {
	ctx := context.Background()
	_, err := wirePosition(ctx, "abc", 1, 6, "utf-16")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "invalid_position" || failure.Message != fmt.Sprintf(prompts.LSPColumnBeyondLine, 6, 1, 4) {
		t.Fatalf("position error changed: %v", err)
	}
	if err.Error() != "invalid_position: "+failure.Message {
		t.Fatalf("error rendering changed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "missing %s.go")
	_, err = readText(ctx, path)
	var pathErr *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("file error no longer wraps underlying failure: %v", err)
	}
	if want := fmt.Sprintf("read LSP file %s: %s; check path and permissions", path, pathErr); err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}

func TestServerErrorGuidanceKeepsDynamicDiagnostic(t *testing.T) {
	c, path, _ := fixture(t, "utf-16", "error")
	_, err := c.Query(queryContext(t), Query{Operation: "hover", Path: path, Line: 1, Column: 1, Limit: 100})
	var failure *Error
	want := fmt.Sprintf(prompts.LSPServerError, "textDocument/hover", -32602, "use a valid path and position[31m")
	if !errors.As(err, &failure) || failure.Code != "invalid_input" || failure.Message != want {
		t.Fatalf("server error diagnostic/guidance changed: %v", err)
	}
}

func TestServerReplyMessagesPreserveExactWording(t *testing.T) {
	for _, test := range []struct {
		name, method, params, want string
	}{
		{"configuration limit", "workspace/configuration", `{"items":[` + strings.TrimSuffix(strings.Repeat(`null,`, 257), ",") + `]}`, `{"error":{"code":-32602,"message":"configuration requires at most 256 items"},"id":"server-id","jsonrpc":"2.0"}`},
		{"invalid configuration", "workspace/configuration", `{"items":"invalid"}`, `{"error":{"code":-32602,"message":"configuration requires at most 256 items"},"id":"server-id","jsonrpc":"2.0"}`},
		{"read-only edits", "workspace/applyEdit", `{"edit":{}}`, `{"id":"server-id","jsonrpc":"2.0","result":{"applied":false,"failureReason":"TTC LSP queries are read-only; use file-mutation tools when available for changes, otherwise report proposed changes"}}`},
		{"unsupported capability", "client/registerCapability", `{}`, `{"error":{"code":-32601,"message":"TTC does not advertise this client capability"},"id":"server-id","jsonrpc":"2.0"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			if err := server.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			c := New(context.Background(), t.TempDir(), client, client)
			defer c.Close()
			request := rpcMessage{Version: "2.0", ID: json.RawMessage(`"server-id"`), Method: test.method, Params: json.RawMessage(test.params)}
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(server, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
				t.Fatal(err)
			}
			data, err = readFrame(bufio.NewReader(server))
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.want {
				t.Fatalf("got %s, want %s", data, test.want)
			}
		})
	}
}

// Guard authored model-facing errors, including framing diagnostics. Outgoing
// Client.answer reply fields are server-facing; fail/errors calls there are not.
// Server-supplied text and underlying I/O failures remain data.
func TestAuthoredFailureGuidanceUsesPromptAssets(t *testing.T) {
	for _, name := range []string{"client.go", "documents.go", "protocol.go", "results.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, pos := range authoredFailureGuidanceViolations(file, name) {
			t.Errorf("%s: authored failure message must use prompts.LSP*", fset.Position(pos))
		}
	}
}

func TestFailureGuidanceAuditOnlyExemptsClientAnswerReplyFields(t *testing.T) {
	for _, test := range []struct {
		name, file, source string
		want               int
	}{
		{"server replies", "protocol.go", `func (c *Client) answer() { _ = map[string]any{"message": "server-only", "failureReason": "server-only"} }`, 0},
		{"answer errors", "protocol.go", `func (c *Client) answer() { _ = fail("code", "model-facing"); _ = errors.New("model-facing"); _ = fmt.Errorf("model-facing") }`, 3},
		{"other method", "protocol.go", `func (c *Client) query() { _ = map[string]any{"message": "model-facing", "failureReason": "model-facing"} }`, 2},
		{"other receiver", "protocol.go", `func (c *Other) answer() { _ = map[string]any{"message": "model-facing"} }`, 1},
		{"other file", "client.go", `func (c *Client) answer() { _ = map[string]any{"message": "model-facing"} }`, 1},
		{"prompt assets", "protocol.go", `func (c *Client) query() { _ = fail("code", prompts.LSPInvalidHeader); _ = errors.New(prompts.LSPInvalidHeader); _ = fmt.Errorf(prompts.LSPReadFileFailed, path, err) }`, 0},
		{"read-loop selected asset", "protocol.go", `func (c *Client) readLoop() { _ = fail("code", message) }`, 0},
		{"other selected message", "protocol.go", `func (c *Client) answer() { _ = fail("code", message) }`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), test.file, "package lsp\n"+test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(authoredFailureGuidanceViolations(file, test.file)); got != test.want {
				t.Fatalf("got %d violations, want %d", got, test.want)
			}
		})
	}
}

func authoredFailureGuidanceViolations(file *ast.File, name string) []token.Pos {
	isAsset := func(expr ast.Expr) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == "prompts" && strings.HasPrefix(selector.Sel.Name, "LSP")
	}
	var violations []token.Pos
	for _, decl := range file.Decls {
		var clientMethod string
		if fn, ok := decl.(*ast.FuncDecl); ok && name == "protocol.go" && fn.Recv != nil && len(fn.Recv.List) == 1 {
			if ptr, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
				if recv, ok := ptr.X.(*ast.Ident); ok && recv.Name == "Client" {
					clientMethod = fn.Name.Name
				}
			}
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			var message ast.Expr
			switch node := node.(type) {
			case *ast.CallExpr:
				if fn, ok := node.Fun.(*ast.Ident); ok && fn.Name == "fail" && len(node.Args) == 2 {
					message = node.Args[1]
				} else if fn, ok := node.Fun.(*ast.SelectorExpr); ok && len(node.Args) > 0 {
					pkg, ok := fn.X.(*ast.Ident)
					if ok && (pkg.Name == "errors" && fn.Sel.Name == "New" || pkg.Name == "fmt" && fn.Sel.Name == "Errorf") {
						message = node.Args[0]
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.BasicLit); ok && (key.Value == `"message"` || key.Value == `"failureReason"`) && clientMethod != "answer" {
					message = node.Value
				}
			}
			if message == nil {
				return true
			}
			if formatted, ok := message.(*ast.CallExpr); ok && len(formatted.Args) > 0 {
				message = formatted.Args[0]
			}
			if variable, ok := message.(*ast.Ident); ok && clientMethod == "readLoop" && variable.Name == "message" {
				return true // readLoop selects its asset before calling fail.
			}
			if !isAsset(message) {
				violations = append(violations, node.Pos())
			}
			return true
		})
	}
	return violations
}
