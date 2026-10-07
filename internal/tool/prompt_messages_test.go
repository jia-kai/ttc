package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp/syntax"
	"strconv"
	"strings"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestEmbeddedToolMessagesReachResults(t *testing.T) {
	r, w, x, _ := toolFixture(t)
	AddShell(r, nil, w, nil)
	AddWeb(r, http.DefaultClient, WebSearchConfig{})
	for _, test := range []struct {
		name, args, code, message string
	}{
		{"absent", `{}`, "unknown_tool", fmt.Sprintf(prompts.ToolUnknown, "absent")},
		{"read", `null`, "invalid_input", prompts.ToolExpectedJSONObject},
		{"read", `{} {}`, "invalid_input", prompts.ToolTrailingJSON},
		{"read", `{"path":"  "}`, "invalid_input", fmt.Sprintf(prompts.ToolRequired, "path")},
		{"read", `{"path":"text","limit":0}`, "invalid_input", fmt.Sprintf(prompts.ToolIntegerRange, "limit", 1, 2000)},
		{"write", `{"path":"text"}`, "invalid_input", prompts.ToolContentRequired},
		{"edit", `{"path":"text","old_text":"x"}`, "invalid_input", prompts.ToolEditTextRequired},
		{"patch", `{"patch_text":"invalid"}`, "invalid_input", prompts.ToolPatchMarkers},
		{"patch", `{"patch_text":"*** Begin Patch\n*** Update File: text\n@@\n-old\n+new\n*** Move to: moved\n*** End Patch"}`, "invalid_input", prompts.ToolPatchMoveHeader},
		{"patch", `{"patch_text":"*** Begin Patch\n*** Update File: text\n*** Move to: \n*** End Patch"}`, "invalid_input", prompts.ToolPatchMoveTarget},
		{"shell", `{"command":"true","timeout_ms":0}`, "invalid_input", prompts.ToolShellForegroundTimeout},
		{"job_stop", `{}`, "invalid_input", Fail("invalid_arguments", prompts.ToolJobStopIdentifier).Error()},
		{"job_stop", `{"child_id":"absent"}`, "not_found", prompts.ToolNoCodingChildren},
		{"lsp_query", `{"job_id":"absent","operation":"hover","path":"text","line":1,"column":1,"limit":1}`, "invalid_input", prompts.ToolLSPHoverPagination},
		{"lsp_query", `{"job_id":"absent","operation":"workspace_symbols","query":"","path":"text","timeout_ms":30000}`, "invalid_input", prompts.ToolLSPWorkspaceArguments},
		{"web_fetch", `{"document_id":"absent"}`, "document_unavailable", prompts.ToolWebDocumentUnavailable},
		{"web_search", `{"query":" "}`, "invalid_input", prompts.ToolSearchQueryRange},
	} {
		t.Run(test.name+"/"+test.code+"/"+test.args, func(t *testing.T) {
			record := r.Invoke(context.Background(), x, test.name, json.RawMessage(test.args))
			assertToolMessage(t, record, test.code, test.message)
		})
	}
}

func TestEmbeddedDiagnosticWrappingAndEncodingFallback(t *testing.T) {
	_, err := searchResult(strings.NewReader(`{"broken":`), false, "")
	var diagnostic *json.SyntaxError
	if !errors.As(err, &diagnostic) || err.Error() != fmt.Errorf(prompts.ToolSearchDecodeResponse, diagnostic).Error() {
		t.Fatalf("search decoding lost wrapped diagnostic: %v", err)
	}
	web := NewRegistry()
	AddWeb(web, http.DefaultClient, WebSearchConfig{})
	fetch, _ := web.Get("web_fetch")
	_, err = fetch.DecodeCall(1, []byte(`{"url":"https://example.test","pattern":"["}`))
	var patternError *syntax.Error
	if !errors.As(err, &patternError) || err.Error() != fmt.Errorf(prompts.ToolWebInvalidPattern, patternError).Error() {
		t.Fatalf("web pattern validation lost wrapped diagnostic: %v", err)
	}
	wrapped := fmt.Errorf(prompts.ToolRGFailed, context.DeadlineExceeded, "raw process diagnostic")
	if !errors.Is(wrapped, context.DeadlineExceeded) || strings.Contains(wrapped.Error(), "%!") {
		t.Fatalf("embedded diagnostic format lost wrapping or argument types: %v", wrapped)
	}

	r := NewRegistry()
	Register(r, "bad_result", "Synthetic result-encoding fixture", nil, nil, func(struct{}) error { return nil }, func(context.Context, Execution, struct{}) (any, error) {
		return Output{Files: []llm.BinaryFile{{Path: "/unused"}}}, &Error{Code: "synthetic", Message: "fixture failure", Details: make(chan int)}
	})
	record := r.Invoke(context.Background(), Execution{}, "bad_result", json.RawMessage(`{}`))
	assertToolMessage(t, record, "execution_failed", prompts.ToolResultEncodingFailed)
	if len(record.Files) != 0 {
		t.Fatalf("encoding failure retained binary attachments: %+v", record.Files)
	}
}

func assertToolMessage(t *testing.T, record Record, code, message string) {
	t.Helper()
	var result struct {
		OK    bool
		Error Error
	}
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatalf("invalid result JSON: %s: %v", record.Result, err)
	}
	if result.OK || result.Error.Code != code || result.Error.Message != message {
		t.Fatalf("result = %s; want code %q, message %q", record.Result, code, message)
	}
}

// TestToolMessageAssetsGuard checks message expressions, not protocol labels or
// dynamic diagnostic arguments. Historical codecs and operator config validation
// have explicit exceptions because those errors are not delivered to models.
func TestToolMessageAssetsGuard(t *testing.T) {
	allowed := map[string]map[string]bool{
		"tool.go:DecodeCall":   {"unsupported tool call version": true},
		"tool.go:DecodeRecord": {"unsupported tool record version": true, "invalid historical tool record": true},
		"web_search.go:Validate": {
			"endpoint: %w": true, "api_key exceeds 4096 bytes": true,
			"api_key must contain only non-space ASCII header characters": true,
		},
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			var checkMessage func(ast.Expr)
			checkMessage = func(expression ast.Expr) {
				switch value := expression.(type) {
				case *ast.BasicLit:
					if value.Kind != token.STRING {
						return
					}
					text, err := strconv.Unquote(value.Value)
					if err != nil {
						t.Fatal(err)
					}
					if !allowed[path+":"+function.Name.Name][text] {
						t.Errorf("%s: authored tool message %q belongs in prompt/tool-messages.yaml", fset.Position(value.Pos()), text)
					}
				case *ast.BinaryExpr:
					checkMessage(value.X)
					checkMessage(value.Y)
				case *ast.ParenExpr:
					checkMessage(value.X)
				case *ast.CallExpr:
					if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sprintf" && len(value.Args) > 0 {
						checkMessage(value.Args[0])
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.CallExpr:
					if name, ok := value.Fun.(*ast.Ident); ok && name.Name == "Fail" && len(value.Args) > 1 {
						checkMessage(value.Args[1])
					}
					if selector, ok := value.Fun.(*ast.SelectorExpr); ok && len(value.Args) > 0 {
						if owner, ok := selector.X.(*ast.Ident); ok && ((owner.Name == "errors" && selector.Sel.Name == "New") || (owner.Name == "fmt" && selector.Sel.Name == "Errorf")) {
							checkMessage(value.Args[0])
						}
					}
				case *ast.CompositeLit:
					if name, ok := value.Type.(*ast.Ident); ok && name.Name == "Error" {
						for _, element := range value.Elts {
							if field, ok := element.(*ast.KeyValueExpr); ok {
								if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Message" {
									checkMessage(field.Value)
								}
							}
						}
					}
				}
				return true
			})
		}
	}
}
