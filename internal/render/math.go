package render

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

func markdownParser() goldmark.Markdown {
	return goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList),
		goldmark.WithParserOptions(
			parser.WithInlineParsers(util.Prioritized(formulaParser{}, 50)),
			parser.WithBlockParsers(util.Prioritized(displayFormulaParser{}, 850))))
}

// Math replaces supported formulas while preserving code, escaped/unmatched
// delimiters and all other source bytes. Formula source is container-normalized.
func Math(input string, replace func(string, bool) string) string {
	return replaceMath(input, func(tex string, display bool, _, _ int) string { return replace(tex, display) })
}

// MathChunkCut moves a proposed source-byte cut before a formula crossing it.
// It scans only the prefix plus the supported 4096-byte formula lookahead;
// unsupported or unfinished formulas keep the original cut.
func MathChunkCut(input string, cut int) int {
	cut = min(max(0, cut), len(input))
	if cut == 0 || cut == len(input) {
		return cut
	}
	prefix := input[:min(len(input), cut+maxFormulaBytes+4)]
	if !strings.ContainsAny(prefix, "$\\") {
		return cut
	}
	result := cut
	replaceMath(prefix, func(_ string, _ bool, start, end int) string {
		if start < cut && cut < end {
			result = start
		}
		return ""
	})
	return result
}

func replaceMath(input string, replace func(string, bool, int, int) string) string {
	source := []byte(input)
	root := markdownParser().Parser().Parse(text.NewReader(source))
	var output strings.Builder
	offset := 0
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		tex, display, start, end := "", false, 0, 0
		switch n := node.(type) {
		case *ast.Image:
			return ast.WalkSkipChildren, nil
		case *formulaNode:
			tex, display, start, end = n.tex, n.display, n.start, n.end
		case *displayFormula:
			tex = n.formula(source)
			if !n.closed || len(tex) > maxFormulaBytes {
				return ast.WalkSkipChildren, nil
			}
			display, start, end = true, n.start, n.end
		default:
			return ast.WalkContinue, nil
		}
		output.WriteString(input[offset:start])
		output.WriteString(replace(tex, display, start, end))
		offset = end
		return ast.WalkSkipChildren, nil
	})
	output.WriteString(input[offset:])
	return output.String()
}

func mathClosingDelimiter(input []byte, delimiter string) int {
	start := 0
	for start < len(input) {
		next := bytes.Index(input[start:], []byte(delimiter))
		if next < 0 {
			return -1
		}
		next += start
		slashes := 0
		for i := next - 1; i >= 0 && input[i] == '\\'; i-- {
			slashes++
		}
		if slashes%2 == 0 {
			return next
		}
		start = next + len(delimiter)
	}
	return -1
}
