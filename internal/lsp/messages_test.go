package lsp

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/prompts"
)

func TestGuidanceTemplatesPreserveFormatting(t *testing.T) {
	for _, test := range []struct {
		name, got, want string
	}{
		{"operation", fmt.Sprintf(prompts.LSPUnsupportedOperation, "hover"), "language server does not advertise hover; choose another operation or server"},
		{"line", fmt.Sprintf(prompts.LSPLineBeyondFile, 7), "line 7 is beyond the file; read it and choose an existing line"},
		{"column", fmt.Sprintf(prompts.LSPColumnBeyondLine, 8, 2, 5), "column 8 exceeds line 2's end column 5; use Unicode code-point columns, not bytes"},
		{"framing", fmt.Sprintf(prompts.LSPInvalidFraming, "bad %s header"), "invalid LSP framing; start with exec SERVER, emit no ordinary stdout, and restart the job: bad %s header"},
		{"server error", fmt.Sprintf(prompts.LSPServerError, "textDocument/hover", -32602, "bad %s position"), "LSP textDocument/hover error -32602: bad %s position; check path/position and server configuration, then retry"},
		{"read-only", prompts.LSPReadOnlyEdits, "TTC LSP queries are read-only; use edit or patch for file changes"},
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

// Guard authored errors, including framing diagnostics and protocol response
// captions. Server-supplied text and underlying I/O failures remain data.
func TestAuthoredFailureGuidanceUsesPromptAssets(t *testing.T) {
	isAsset := func(expr ast.Expr) bool {
		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == "prompts" && strings.HasPrefix(selector.Sel.Name, "LSP")
	}
	for _, name := range []string{"client.go", "documents.go", "protocol.go", "results.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
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
				if key, ok := node.Key.(*ast.BasicLit); ok && (key.Value == `"message"` || key.Value == `"failureReason"`) {
					message = node.Value
				}
			}
			if message == nil {
				return true
			}
			if formatted, ok := message.(*ast.CallExpr); ok && len(formatted.Args) > 0 {
				message = formatted.Args[0]
			}
			if variable, ok := message.(*ast.Ident); ok && name == "protocol.go" && variable.Name == "message" {
				return true // readLoop selects its asset before calling fail.
			}
			if !isAsset(message) {
				t.Errorf("%s: authored failure message must use prompts.LSP*", fset.Position(node.Pos()))
			}
			return true
		})
	}
}
