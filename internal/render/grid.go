package render

import (
	"bytes"
	"fmt"
	"html"
	"strings"

	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/util"
)

// gridRenderer owns geometry. Styles describe attributes and decorations, never
// implicit buffers or cursor positions. Each recursive container receives its
// actual content width, then prefixes every returned row (including blank rows).
type gridRenderer struct {
	source []byte
	style  glamouransi.StyleConfig
	assets AssetLayout
}

func mergeStyle(parent, child glamouransi.StylePrimitive) glamouransi.StylePrimitive {
	if child.Color != nil {
		parent.Color = child.Color
	}
	if child.BackgroundColor != nil {
		parent.BackgroundColor = child.BackgroundColor
	}
	if child.Bold != nil {
		parent.Bold = child.Bold
	}
	if child.Italic != nil {
		parent.Italic = child.Italic
	}
	if child.Underline != nil {
		parent.Underline = child.Underline
	}
	if child.CrossedOut != nil {
		parent.CrossedOut = child.CrossedOut
	}
	if child.Faint != nil {
		parent.Faint = child.Faint
	}
	if child.Conceal != nil {
		parent.Conceal = child.Conceal
	}
	if child.Inverse != nil {
		parent.Inverse = child.Inverse
	}
	if child.Blink != nil {
		parent.Blink = child.Blink
	}
	return parent
}

func styled(value string, p glamouransi.StylePrimitive) string {
	s := ansi.Style{}
	if p.Color != nil {
		s = s.ForegroundColor(lipgloss.Color(*p.Color))
	}
	if p.BackgroundColor != nil {
		s = s.BackgroundColor(lipgloss.Color(*p.BackgroundColor))
	}
	if p.Bold != nil && *p.Bold {
		s = s.Bold()
	}
	if p.Italic != nil && *p.Italic {
		s = s.Italic(true)
	}
	if p.Underline != nil && *p.Underline {
		s = s.Underline(true)
	}
	if p.CrossedOut != nil && *p.CrossedOut {
		s = s.Strikethrough(true)
	}
	if p.Faint != nil && *p.Faint {
		s = s.Faint()
	}
	if p.Conceal != nil && *p.Conceal {
		s = s.Conceal(true)
	}
	if p.Inverse != nil && *p.Inverse {
		s = s.Reverse(true)
	}
	if p.Blink != nil && *p.Blink {
		s = s.Blink(true)
	}
	return s.Styled(value)
}

func decoded(value []byte) string {
	return Clean(html.UnescapeString(string(util.UnescapePunctuations(value))))
}

func (g *gridRenderer) imageText(image *ast.Image) string {
	var value strings.Builder
	_ = ast.Walk(image, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := node.(type) {
		case *formulaNode:
			value.Write(n.Text(g.source))
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if n.IsRaw() {
				value.Write(n.Text(g.source))
			} else {
				value.WriteString(decoded(n.Text(g.source)))
			}
			if n.HardLineBreak() || n.SoftLineBreak() {
				value.WriteByte(' ')
			}
		case *ast.String:
			if n.IsRaw() {
				value.Write(n.Value)
			} else {
				value.WriteString(decoded(n.Value))
			}
		}
		return ast.WalkContinue, nil
	})
	return value.String()
}

func (g *gridRenderer) inline(parent ast.Node, style glamouransi.StylePrimitive) []flowItem {
	var items []flowItem
	g.appendInline(parent, style, &items)
	return items
}

func (g *gridRenderer) appendInline(parent ast.Node, style glamouransi.StylePrimitive, items *[]flowItem) {
	text := func(value string, style glamouransi.StylePrimitive) {
		if value != "" {
			*items = append(*items, flowItem{text: styled(value, style)})
		}
	}
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		switch n := n.(type) {
		case *formulaNode:
			*items = append(*items, assetItem(g.assets, Asset{Kind: MathAsset, Source: n.tex, Display: n.display}))
		case *ast.Text:
			value := string(n.Segment.Value(g.source))
			if !n.IsRaw() {
				value = decoded([]byte(value))
			}
			text(value, style)
			if n.HardLineBreak() || n.SoftLineBreak() {
				text("\n", style)
			}
		case *ast.String:
			value := string(n.Value)
			if !n.IsRaw() {
				value = decoded(n.Value)
			}
			text(value, style)
		case *ast.CodeSpan:
			var code strings.Builder
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				code.Write(c.Text(g.source))
			}
			p := g.style.Code.StylePrimitive
			text(p.Prefix+strings.ReplaceAll(code.String(), "\n", " ")+p.Suffix, mergeStyle(style, p))
		case *ast.Emphasis:
			p := g.style.Emph
			if n.Level == 2 {
				p = g.style.Strong
			}
			g.appendInline(n, mergeStyle(style, p), items)
		case *extast.Strikethrough:
			g.appendInline(n, mergeStyle(style, g.style.Strikethrough), items)
		case *ast.Link:
			g.appendInline(n, mergeStyle(style, g.style.LinkText), items)
			if len(n.Destination) > 0 {
				text(" "+decoded(n.Destination), mergeStyle(style, g.style.Link))
			}
		case *ast.AutoLink:
			text(string(n.URL(g.source)), mergeStyle(style, g.style.Link))
		case *ast.Image:
			*items = append(*items, assetItem(g.assets, Asset{Kind: ImageAsset, Source: decoded(n.Destination), Alt: g.imageText(n)}))
		case *extast.TaskCheckBox: // The enclosing list item owns the checkbox prefix.
		case *ast.RawHTML:
			for i := 0; i < n.Segments.Len(); i++ {
				segment := n.Segments.At(i)
				text(string(segment.Value(g.source)), mergeStyle(style, g.style.HTMLSpan.StylePrimitive))
			}
		default:
			g.appendInline(n, style, items)
		}
	}
}

func prefixBox(box FixedBox, first, rest string, baseline bool) FixedBox {
	markerRow := 0
	if baseline {
		markerRow = box.Baseline
	}
	for i := range box.Rows {
		prefix := rest
		if i == markerRow {
			prefix = first
		}
		box.Rows[i] = prefix + box.Rows[i]
	}
	box.Width += ansi.StringWidth(rest)
	return box
}

// prefixWidth reserves room for at least one two-cell grapheme in tiny panes.
func prefixWidth(prefix string, width int) string { return ansi.Truncate(prefix, max(0, width-2), "") }

func (g *gridRenderer) children(parent ast.Node, width int, tight bool) (FixedBox, error) {
	box := FixedBox{Width: width}
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		block, err := g.block(n, width)
		if err != nil {
			return FixedBox{}, err
		}
		if len(block.Rows) == 0 {
			continue
		}
		if len(box.Rows) == 0 {
			box.Baseline = block.Baseline
		}
		if len(box.Rows) > 0 && !tight {
			box.Rows = append(box.Rows, "")
		}
		box.Rows = append(box.Rows, block.Rows...)
	}
	return box, nil
}

func (g *gridRenderer) block(node ast.Node, width int) (FixedBox, error) {
	width = max(1, width)
	base := g.style.Document.StylePrimitive
	switch n := node.(type) {
	case *displayFormula:
		value := n.formula(g.source)
		if n.closed && len(value) <= maxFormulaBytes {
			box := layoutFlowBox([]flowItem{assetItem(g.assets, Asset{Kind: MathAsset, Source: value, Display: true})}, width)
			box.Baseline = 0 // A display block has no surrounding prose baseline.
			return box, nil
		}
		if n.closed {
			value += n.close
		}
		return textBlock(styled(n.open+value, base), width), nil
	case *ast.Document:
		return g.children(n, width, false)
	case *ast.Paragraph, *ast.TextBlock:
		return layoutFlowBox(g.inline(n, mergeStyle(base, g.style.Paragraph.StylePrimitive)), width), nil
	case *ast.Heading:
		headings := []glamouransi.StyleBlock{g.style.H1, g.style.H2, g.style.H3, g.style.H4, g.style.H5, g.style.H6}
		h := headings[n.Level-1]
		style := mergeStyle(mergeStyle(base, g.style.Heading.StylePrimitive), h.StylePrimitive)
		prefix := prefixWidth(styled(h.Prefix, style), width)
		box := layoutFlowBox(g.inline(n, style), max(1, width-ansi.StringWidth(prefix)))
		return prefixBox(box, prefix, strings.Repeat(" ", ansi.StringWidth(prefix)), true), nil
	case *ast.Blockquote:
		token := "│ "
		if g.style.BlockQuote.IndentToken != nil {
			token = *g.style.BlockQuote.IndentToken
		}
		prefix := prefixWidth(styled(token, mergeStyle(base, g.style.BlockQuote.StylePrimitive)), width)
		rows, err := g.children(n, width-ansi.StringWidth(prefix), false)
		return prefixBox(rows, prefix, prefix, false), err
	case *ast.List:
		result := FixedBox{Width: width}
		index := n.Start
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			marker := g.style.Item.BlockPrefix
			if n.IsOrdered() {
				marker = fmt.Sprint(index) + g.style.Enumeration.BlockPrefix
			}
			if first := item.FirstChild(); first != nil {
				if check, ok := first.FirstChild().(*extast.TaskCheckBox); ok {
					marker = g.style.Task.Unticked
					if check.IsChecked {
						marker = g.style.Task.Ticked
					}
				}
			}
			prefix := prefixWidth(styled(marker, base), width)
			block, err := g.children(item, width-ansi.StringWidth(prefix), n.IsTight)
			if err != nil {
				return FixedBox{}, err
			}
			if len(block.Rows) == 0 {
				block.Rows = []string{""}
			}
			if len(result.Rows) == 0 {
				result.Baseline = block.Baseline
			}
			if len(result.Rows) > 0 && !n.IsTight {
				result.Rows = append(result.Rows, "")
			}
			result.Rows = append(result.Rows, prefixBox(block, prefix, strings.Repeat(" ", ansi.StringWidth(prefix)), true).Rows...)
			index++
		}
		return result, nil
	case *extast.Table:
		var cells [][][]flowItem
		for row := n.FirstChild(); row != nil; row = row.NextSibling() {
			var values [][]flowItem
			style := base
			if row.Kind() == extast.KindTableHeader {
				style = mergeStyle(base, g.style.Strong)
			}
			for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
				values = append(values, g.inline(cell, style))
			}
			cells = append(cells, values)
		}
		alignments := make([]int, len(n.Alignments))
		for i, a := range n.Alignments {
			alignments[i] = -1
			if a == extast.AlignCenter {
				alignments[i] = 0
			}
			if a == extast.AlignRight {
				alignments[i] = 1
			}
		}
		return FixedBox{Rows: layoutTable(cells, alignments, width), Width: width}, nil
	case *ast.FencedCodeBlock:
		return g.code(n, n.Language(g.source), width)
	case *ast.CodeBlock:
		return g.code(n, nil, width)
	case *ast.ThematicBreak:
		return FixedBox{Rows: []string{styled(strings.Repeat("─", width), g.style.HorizontalRule)}, Width: width}, nil
	case *extast.DefinitionList:
		return g.children(n, width, true)
	case *extast.DefinitionTerm:
		return layoutFlowBox(g.inline(n, mergeStyle(base, g.style.DefinitionTerm)), width), nil
	case *extast.DefinitionDescription:
		prefix := prefixWidth(styled(strings.Trim(g.style.DefinitionDescription.BlockPrefix, "\n"), base), width)
		rows, err := g.children(n, width-ansi.StringWidth(prefix), false)
		return prefixBox(rows, prefix, strings.Repeat(" ", ansi.StringWidth(prefix)), true), err
	case *ast.HTMLBlock:
		var value strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			segment := n.Lines().At(i)
			value.Write(segment.Value(g.source))
		}
		if n.HasClosure() {
			value.Write(n.ClosureLine.Value(g.source))
		}
		return textBlock(styled(strings.TrimSuffix(value.String(), "\n"), base), width), nil
	default:
		return g.children(n, width, false)
	}
}

func (g *gridRenderer) code(node ast.Node, language []byte, width int) (FixedBox, error) {
	var code strings.Builder
	for i := 0; i < node.Lines().Len(); i++ {
		segment := node.Lines().At(i)
		code.Write(segment.Value(g.source))
	}
	style := g.style
	prefix := ""
	if style.CodeBlock.Indent != nil {
		prefix = prefixWidth(strings.Repeat(" ", int(*style.CodeBlock.Indent)), width)
	}
	zero := uint(0)
	style.CodeBlock.Margin = &zero
	style.CodeBlock.Indent = &zero
	ctx := glamouransi.NewRenderContext(glamouransi.Options{Styles: style})
	var ignored, output bytes.Buffer
	document := glamouransi.BlockElement{Block: &bytes.Buffer{}, Style: style.Document}
	if err := document.Render(&ignored, ctx); err != nil {
		return FixedBox{}, err
	}
	element := glamouransi.CodeBlockElement{Code: code.String(), Language: string(language)}
	if err := element.Render(&output, ctx); err != nil {
		return FixedBox{}, err
	}
	value := strings.TrimSuffix(output.String(), "\n")
	// Hardwrap preserves indentation and whitespace, unlike prose word packing.
	return prefixBox(textBlock(value, width-ansi.StringWidth(prefix)), prefix, prefix, false), nil
}

// textBlock preserves code/HTML whitespace and makes each output row's SGR
// attributes independent of preceding rows and container prefixes.
func textBlock(value string, width int) FixedBox {
	box := FixedBox{Width: width}
	var row strings.Builder
	for token := range flowTokens(ansi.Hardwrap(value, width, true)) {
		if token.line {
			box.Rows = append(box.Rows, row.String())
			row.Reset()
		} else {
			row.WriteString(token.text)
		}
	}
	box.Rows = append(box.Rows, row.String())
	return box
}
