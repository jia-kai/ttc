package render

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"sort"
	"strings"
	"unicode"
)

// Math replaces inline/block formulas while preserving code, escaped delimiters,
// and unmatched delimiters. The caller owns typesetting and fallback behavior.
func Math(input string, replace func(string, bool) string) string {
	var out strings.Builder
	protected := mathCodeBlocks(input)
	blockIndex := 0
	for i := 0; i < len(input); {
		for blockIndex < len(protected) && protected[blockIndex][1] <= i {
			blockIndex++
		}
		if blockIndex < len(protected) && protected[blockIndex][0] <= i {
			end := protected[blockIndex][1]
			out.WriteString(input[i:end])
			i = end
			continue
		}
		if input[i] == '`' {
			end := i + 1
			for end < len(input) && input[end] == '`' {
				end++
			}
			delimiter := input[i:end]
			if close := strings.Index(input[end:], delimiter); close >= 0 {
				end += close + len(delimiter)
			} else {
				end = len(input)
			}
			out.WriteString(input[i:end])
			i = end
			continue
		}
		if input[i] == '\\' && i+1 < len(input) && (input[i+1] == '$' || input[i+1] == '`' || input[i+1] == '\\') {
			out.WriteString(input[i : i+2])
			i += 2
			continue
		}
		open, close, block := "", "", false
		switch {
		case strings.HasPrefix(input[i:], "$$"):
			open, close, block = "$$", "$$", true
		case strings.HasPrefix(input[i:], `\[`):
			open, close, block = `\[`, `\]`, true
		case strings.HasPrefix(input[i:], `\(`):
			open, close = `\(`, `\)`
		case input[i] == '$' && i+1 < len(input) && !unicode.IsSpace(rune(input[i+1])):
			open, close = "$", "$"
		}
		if open != "" {
			start := i + len(open)
			end := mathClosingDelimiter(input[start:], close)
			if end >= 0 {
				end += start
				formula := input[start:end]
				if block || (!strings.Contains(formula, "\n") && formula != "" && !unicode.IsSpace(rune(formula[len(formula)-1]))) {
					out.WriteString(replace(formula, block))
					i = end + len(close)
					continue
				}
			}
		}
		out.WriteByte(input[i])
		i++
	}
	return out.String()
}

func mathCodeBlocks(input string) [][2]int {
	source := []byte(input)
	root := goldmark.New().Parser().Parse(text.NewReader(source))
	var ranges [][2]int
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (n.Kind() == ast.KindFencedCodeBlock || n.Kind() == ast.KindCodeBlock) && n.Lines().Len() > 0 {
			ranges = append(ranges, [2]int{n.Lines().At(0).Start, n.Lines().At(n.Lines().Len() - 1).Stop})
		}
		return ast.WalkContinue, nil
	})
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	return ranges
}

func mathClosingDelimiter(input, delimiter string) int {
	start := 0
	for start < len(input) {
		next := strings.Index(input[start:], delimiter)
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
