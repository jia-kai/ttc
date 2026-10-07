package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var proseWord = regexp.MustCompile(`[A-Za-z]{2,}`)

// authoredErrorLiterals finds prose at tool-result error sinks. Error codes,
// punctuation-only formatting and dynamic diagnostics are not authored prompts.
func authoredErrorLiterals(file *ast.File) []token.Pos {
	var found []token.Pos
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if name != "Fail" && name != "fail" && name != "failedTool" {
			return true
		}
		message := len(call.Args) - 1
		ast.Inspect(call.Args[message], func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err == nil && proseWord.MatchString(text) {
				found = append(found, literal.Pos())
			}
			return true
		})
		return true
	})
	return found
}

func TestSessionToolResultGuidanceUsesPromptAssets(t *testing.T) {
	root := filepath.Join("..", "..")
	// Tool and LSP packages have stricter local guards with their own boundary
	// exceptions; cover the session-owned tool and interrupted-result sinks here.
	for _, directory := range []string{"internal/session"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, directory, entry.Name())
			positions := token.NewFileSet()
			file, err := parser.ParseFile(positions, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, pos := range authoredErrorLiterals(file) {
				t.Errorf("%s: authored model-facing guidance belongs in prompt/", positions.Position(pos))
			}
		}
	}
}

func TestPromptAuditDistinguishesProseAndData(t *testing.T) {
	for _, tc := range []struct {
		expression string
		want       int
	}{
		{`tool.Fail("invalid", "read current contents before retrying")`, 1},
		{`Fail("invalid", fmt.Sprintf("choose one of %s", value))`, 1},
		{`fail("invalid", prompts.LSPPathRequired)`, 0},
		{`failedTool(call, "interrupted", prompts.StreamInterruptedTool)`, 0},
		{`tool.Fail("invalid", err.Error())`, 0},
		{`tool.Fail("invalid", fmt.Sprintf("%s: %v", prompts.ToolUnknown, err))`, 0},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package fixture; func f() { _ = "+tc.expression+" }", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(authoredErrorLiterals(file)); got != tc.want {
			t.Errorf("%s: got %d authored literals, want %d", tc.expression, got, tc.want)
		}
	}
}
