package render

import (
	"bytes"
	"strings"

	glamouransi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Terminal formats Markdown for a terminal viewport using a fixed, deterministic
// style. Table headers use colored strong emphasis. Input controls are removed;
// math retains readable TeX; graphics frontends replace formulas before rendering.
func Terminal(input string, width int) (string, error) {
	style := markdownStyle()
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.DefinitionList),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	// Register the ANSI renderer after extensions, so their HTML renderers cannot
	// override GFM tables, checkboxes or definition lists.
	md.SetRenderer(renderer.NewRenderer(renderer.WithNodeRenderers(util.Prioritized(
		glamouransi.NewRenderer(glamouransi.Options{Styles: style, WordWrap: max(8, width), PreserveNewLines: true}), 1000,
	))))
	source := []byte(Math(Clean(input), func(tex string, block bool) string {
		if block {
			return "\n\n" + Fence(tex, "tex") + "\n\n"
		}
		f := backticks(tex, 1)
		return f + " " + tex + " " + f
	}))
	root := md.Parser().Parse(text.NewReader(source))
	// Style parsed header cells rather than matching pipe-delimited source lines;
	// escaped pipes, inline formatting and fenced examples keep their semantics.
	if err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && n.Kind() == extast.KindTableHeader {
			for cell := n.FirstChild(); cell != nil; cell = cell.NextSibling() {
				strong := ast.NewEmphasis(2)
				for cell.HasChildren() {
					strong.AppendChild(strong, cell.FirstChild())
				}
				cell.AppendChild(cell, strong)
			}
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return "", err
	}
	var output bytes.Buffer
	if err := md.Renderer().Render(&output, source, root); err != nil {
		return "", err
	}
	return wrapIndented(output.String(), max(1, width)), nil
}

// wrapIndented keeps continuation rows aligned with their source row's indent.
// Wrapping affects presentation only; the JSON/code source remains unchanged.
func wrapIndented(text string, width int) string {
	rows := strings.Split(text, "\n")
	for i, row := range rows {
		if ansi.StringWidth(row) <= width {
			continue
		}
		plain := ansi.Strip(row)
		indent := len(plain) - len(strings.TrimLeft(plain, " "))
		body := ansi.Cut(row, indent, ansi.StringWidth(row))
		prefix := strings.Repeat(" ", min(indent, width/2))
		body = ansi.Wrap(body, width-len(prefix), "")
		rows[i] = prefix + strings.ReplaceAll(body, "\n", "\n"+prefix)
	}
	// Wrap can leave styled trailing padding wider than very narrow viewports.
	return ansi.Hardwrap(strings.Join(rows, "\n"), width, true)
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
		if strings.TrimSpace(ansi.Strip(row)) != "" {
			// Glamour pads code blocks with styled spaces. Trim by cells while
			// retaining the SGR sequences needed by command highlighting.
			plain := ansi.Strip(row)
			left := len([]rune(plain)) - len([]rune(strings.TrimLeft(plain, " ")))
			parts = append(parts, ansi.Cut(row, left, ansi.StringWidth(strings.TrimRight(plain, " "))))
		}
	}
	text = strings.Join(parts, " ")
	if !colors {
		text = ansi.Strip(text)
	}
	return ansi.Truncate(text, max(1, width), "…"), nil
}
