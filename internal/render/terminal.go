package render

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Terminal formats control-safe Markdown for a terminal-cell viewport. Formulas
// retain readable TeX and images retain their labels/destinations, without I/O.
func Terminal(input string, width int) (string, error) {
	return TerminalWithAssets(input, width, LiteralAsset)
}

// TerminalWithAssets lays out Markdown with atomic math and image boxes.
// Every block, including tables without formulas, shares the same grid layout.
// Containers reserve their prefixes before laying out their complete contents.
func TerminalWithAssets(input string, width int, layout AssetLayout) (string, error) {
	return ParseMarkdown(input).Render(width, layout)
}

// Document is an immutable, control-safe Markdown syntax tree. Reuse it across
// viewport resizes and asset revisions; each Render owns its layout state.
type Document struct {
	source []byte
	root   ast.Node
}

// ParseMarkdown sanitizes input and parses Markdown and container-aware math.
// Unsupported or unfinished formulas remain literal Markdown text.
func ParseMarkdown(input string) *Document {
	source := []byte(Clean(input))
	return &Document{source: source, root: markdownParser().Parser().Parse(text.NewReader(source))}
}

// Render lays out the document at width terminal cells using layout for atomic
// assets. Nonpositive widths become one; indivisible wide graphemes and boxes
// may overflow intact. A nil layout is an error; use LiteralAsset for text fallback.
func (d *Document) Render(width int, layout AssetLayout) (string, error) {
	if layout == nil {
		return "", fmt.Errorf("Markdown asset layout is required")
	}
	if d == nil || d.root == nil {
		return "", fmt.Errorf("Markdown document is not initialized")
	}
	grid := gridRenderer{source: d.source, style: markdownStyle(), assets: layout}
	rows, err := grid.block(d.root, max(1, width))
	if err != nil {
		return "", err
	}
	return strings.Join(rows.Rows, "\n"), nil
}

// LiteralBox folds whitespace and wraps control-safe plain text into a terminal
// rectangle. It retains the complete text for pending/failed graphics fallbacks.
// A grapheme wider than width is preserved intact rather than split or dropped.
func LiteralBox(text string, width int) FixedBox {
	rows := layoutFlow([]flowItem{{text: strings.Join(strings.Fields(Clean(text)), " ")}}, max(1, width))
	columns := 1
	for _, row := range rows {
		columns = max(columns, ansi.StringWidth(row))
	}
	return FixedBox{Rows: rows, Width: columns, Baseline: len(rows) - 1}
}

// TerminalBriefing renders portable Markdown as one row, preserving generated
// highlight styles and clipping by terminal cells. Inspectors retain full text.
// colors=false returns plain text; width is terminal cells, clamped to at least one.
func TerminalBriefing(input string, width int, colors bool) (string, error) {
	text, err := Terminal(input, max(512, width))
	if err != nil {
		return "", err
	}
	var parts []string
	for _, row := range strings.Split(text, "\n") {
		plain := ansi.Strip(row)
		if strings.TrimSpace(plain) != "" {
			left := ansi.StringWidth(plain) - ansi.StringWidth(strings.TrimLeft(plain, " "))
			parts = append(parts, ansi.Cut(row, left, ansi.StringWidth(strings.TrimRight(plain, " "))))
		}
	}
	text = strings.Join(parts, " ")
	if !colors {
		text = ansi.Strip(text)
	}
	return ansi.Truncate(text, max(1, width), "…"), nil
}
