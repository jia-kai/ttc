package render

import (
	"bytes"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"

	glamouransi "charm.land/glamour/v2/ansi"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// MathLayout fits a formula to width terminal columns. Display identifies block
// delimiters; the returned box is indivisible to surrounding text layout.
// Calls may include measurement passes; callers must not start asynchronous work
// until the resulting rows are known to intersect the viewport.
type MathLayout func(tex string, display bool, width int) FixedBox

type formulaToken struct {
	tex     string
	display bool
	source  string
}

type markdownBoxes struct {
	formulas map[rune]formulaToken
	carriers map[rune]blockCarrier
	used     map[rune]bool
	next     rune
	layout   MathLayout
	width    int
}

type blockCarrier struct {
	layout     func(int) []string
	listIndent int
	decoration string
}

func continuationPrefix(prefix, decoration string) string {
	plain := ansi.Strip(prefix)
	if decoration == "" || !strings.HasSuffix(plain, decoration) {
		return prefix
	}
	width := ansi.StringWidth(decoration)
	return ansi.Cut(prefix, 0, ansi.StringWidth(plain)-width) + strings.Repeat(" ", width)
}

func newMarkdownBoxes(input string, width int, layout MathLayout) *markdownBoxes {
	used := map[rune]bool{}
	// Glamour decodes HTML entities during inline rendering. Reserve their
	// resulting runes as well as literal input before allocating opaque tokens.
	for _, r := range html.UnescapeString(input) {
		if r >= 0xe000 && r <= 0xf8ff {
			used[r] = true
		}
	}
	return &markdownBoxes{formulas: map[rune]formulaToken{}, carriers: map[rune]blockCarrier{}, used: used, next: 0xe000, layout: layout, width: width}
}

func listDecoration(node ast.Node) (string, error) {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if parent.Kind() != ast.KindListItem {
			continue
		}
		options := glamouransi.Options{Styles: markdownStyle()}
		ctx := glamouransi.NewRenderContext(options)
		var out bytes.Buffer
		if first := parent.FirstChild(); first != nil {
			if check, ok := first.FirstChild().(*extast.TaskCheckBox); ok {
				err := (&glamouransi.TaskElement{Checked: check.IsChecked}).Render(&out, ctx)
				return ansi.Strip(out.String()), err
			}
		}
		list := parent.Parent().(*ast.List)
		enumeration := uint(max(1, list.Start))
		for previous := parent.PreviousSibling(); previous != nil; previous = previous.PreviousSibling() {
			enumeration++
		}
		err := (&glamouransi.ItemElement{IsOrdered: list.IsOrdered(), Enumeration: enumeration}).Render(&out, ctx)
		return ansi.Strip(out.String()), err
	}
	return "", nil
}

func (b *markdownBoxes) marker() (rune, error) {
	for b.next <= 0xf8ff {
		r := b.next
		b.next++
		if !b.used[r] {
			return r, nil
		}
	}
	return 0, fmt.Errorf("too many Markdown math placeholders")
}

func (b *markdownBoxes) tokenize(input string) (string, error) {
	var failure error
	source := replaceMath(input, func(tex string, display bool, start, end int) string {
		marker, err := b.marker()
		if err != nil {
			failure = err
			return tex
		}
		b.formulas[marker] = formulaToken{tex: tex, display: display, source: input[start:end]}
		return string(marker)
	})
	return source, failure
}

// prepare replaces whole math-containing blocks with one-cell carriers. Glamour
// lays out those carriers normally, retaining buffer order and container prefixes.
// Only after its final wrapping do we expand them at the actual remaining width.
func (b *markdownBoxes) prepare(root ast.Node, source *[]byte) error {
	// A dollar in a URL/title is not mathematical content. Restore these fields
	// before either Glamour or our cell adapter renders their presentation.
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch link := n.(type) {
			case *ast.Link:
				link.Destination = []byte(b.restore(string(link.Destination)))
				link.Title = []byte(b.restore(string(link.Title)))
			case *ast.Image:
				link.Destination = []byte(b.restore(string(link.Destination)))
				link.Title = []byte(b.restore(string(link.Title)))
			case *ast.AutoLink:
				label := string(link.Label(*source))
				url := string(link.URL(*source))
				if b.hasFormula(label) || b.hasFormula(url) {
					replacement := ast.NewLink()
					replacement.Destination = []byte(b.restore(url))
					start := len(*source)
					*source = append(*source, b.restore(label)...)
					replacement.AppendChild(replacement, ast.NewTextSegment(text.NewSegment(start, len(*source))))
					link.Parent().ReplaceChild(link.Parent(), link, replacement)
					return ast.WalkSkipChildren, nil
				}
			}
		}
		return ast.WalkContinue, nil
	})
	var nodes []ast.Node
	if err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case extast.KindTable, ast.KindParagraph, ast.KindTextBlock, ast.KindHeading, extast.KindDefinitionTerm:
			if b.hasFormula(string(n.Text(*source))) {
				nodes = append(nodes, n)
				return ast.WalkSkipChildren, nil
			}
		}
		return ast.WalkContinue, nil
	}); err != nil {
		return err
	}
	for _, node := range nodes {
		var render func(int) []string
		if node.Kind() == extast.KindTable {
			var cells [][][]flowItem
			for row := node.FirstChild(); row != nil; row = row.NextSibling() {
				var values [][]flowItem
				for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
					items, err := b.inline(cell, *source)
					if err != nil {
						return err
					}
					values = append(values, items)
				}
				cells = append(cells, values)
			}
			var alignments []int
			for _, alignment := range node.(*extast.Table).Alignments {
				align := -1
				if alignment == extast.AlignCenter {
					align = 0
				}
				if alignment == extast.AlignRight {
					align = 1
				}
				alignments = append(alignments, align)
			}
			render = func(width int) []string { return layoutTable(cells, alignments, width) }
		} else {
			items, err := b.inline(node, *source)
			if err != nil {
				return err
			}
			render = func(width int) []string { return layoutFlow(items, width) }
		}
		marker, err := b.marker()
		if err != nil {
			return err
		}
		list, err := listDecoration(node)
		if err != nil {
			return err
		}
		decoration := list
		style := markdownStyle()
		if heading, ok := node.(*ast.Heading); ok {
			headings := []glamouransi.StyleBlock{style.H1, style.H2, style.H3, style.H4, style.H5, style.H6}
			decoration = headings[heading.Level-1].Prefix
		} else if node.Parent().Kind() == extast.KindDefinitionDescription {
			decoration = strings.TrimLeft(style.DefinitionDescription.BlockPrefix, "\n")
		}
		b.carriers[marker] = blockCarrier{layout: render, listIndent: ansi.StringWidth(list), decoration: decoration}
		start := len(*source)
		carrier := string(marker)
		if node.Kind() == extast.KindTable {
			// Tight list paragraphs do not add their own block separators.
			carrier = "\n" + carrier + "\n"
		} else if node.Parent().Kind() == ast.KindListItem {
			// Glamour's tight/loose list paragraphs share a parent buffer. Explicit
			// boundaries keep real preceding/following prose out of carrier rows.
			if node.PreviousSibling() != nil {
				carrier = "\n" + carrier
			}
			if node.NextSibling() != nil {
				carrier += "\n"
			}
		}
		*source = append(*source, []byte(carrier)...)
		replacement := ast.NewParagraph()
		replacement.AppendChild(replacement, ast.NewTextSegment(text.NewSegment(start, len(*source))))
		// Preserve heading and text-block container behavior; table carriers are
		// paragraphs so the parent renderer owns their spacing and indentation.
		if node.Kind() != extast.KindTable {
			for child := node.FirstChild(); child != nil; {
				next := child.NextSibling()
				// Glamour reads this metadata when rendering the parent item.
				if child.Kind() != extast.KindTaskCheckBox {
					node.RemoveChild(node, child)
				}
				child = next
			}
			node.AppendChild(node, ast.NewTextSegment(text.NewSegment(start, len(*source))))
		} else {
			node.Parent().ReplaceChild(node.Parent(), node, replacement)
		}
	}
	return nil
}

func (b *markdownBoxes) hasFormula(s string) bool {
	for _, r := range s {
		if _, ok := b.formulas[r]; ok {
			return true
		}
	}
	return false
}

func (b *markdownBoxes) restore(s string) string {
	if !b.hasFormula(s) {
		return s
	}
	var out strings.Builder
	for _, r := range s {
		if formula, ok := b.formulas[r]; ok {
			out.WriteString(formula.source)
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func (b *markdownBoxes) inline(node ast.Node, source []byte) ([]flowItem, error) {
	options := glamouransi.Options{Styles: markdownStyle(), WordWrap: max(8, b.width), PreserveNewLines: true, InlineTableLinks: true}
	renderer := glamouransi.NewRenderer(options)
	ctx := glamouransi.NewRenderContext(options)
	var ignored bytes.Buffer
	document := glamouransi.BlockElement{Block: &bytes.Buffer{}, Style: options.Styles.Document}
	if err := document.Render(&ignored, ctx); err != nil {
		return nil, err
	}
	if heading, ok := node.(*ast.Heading); ok {
		if err := (&glamouransi.HeadingElement{Level: heading.Level, First: true}).Render(&ignored, ctx); err != nil {
			return nil, err
		}
	}
	var output bytes.Buffer
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		element := renderer.NewElement(child, source)
		if element.Renderer == nil {
			continue
		}
		err := element.Renderer.Render(&output, ctx)
		if err != nil {
			return nil, fmt.Errorf("render Markdown cell content: %w", err)
		}
	}
	return b.inlineItems(output.String(), node.Kind() == extast.KindTableCell), nil
}

// inlineItems extracts formula markers in one pass. Each text fragment starts
// with canonical SGR state and ends reset, so boxes cannot disturb its styling.
// Prose boxes are lazy; table cells additionally measure their natural width.
func (b *markdownBoxes) inlineItems(rendered string, tableCell bool) []flowItem {
	var items []flowItem
	var fragment strings.Builder
	var style uv.Style
	var parser ansi.Parser
	parser.SetParamsSize(32)
	write := func(s string) {
		if s == "" {
			return
		}
		if fragment.Len() == 0 && !style.IsZero() {
			fragment.WriteString(style.String())
		}
		fragment.WriteString(s)
	}
	flush := func() {
		if fragment.Len() == 0 {
			return
		}
		s := fragment.String()
		if strings.Contains(s, "\x1b[") {
			s += ansi.ResetStyle
		}
		items = append(items, flowItem{text: s})
		fragment.Reset()
	}
	var state byte
	for len(rendered) > 0 {
		seq, _, n, next := ansi.DecodeSequence(rendered, state, &parser)
		rendered, state = rendered[n:], next
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			uv.ReadStyle(parser.Params(), &style)
			if fragment.Len() > 0 {
				fragment.WriteString(seq)
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(seq)
		formula, ok := b.formulas[r]
		if !ok {
			write(seq)
			continue
		}
		flush()
		// Measurement and final layout can revisit a width. Reuse each exact
		// rectangle, including its graphics markers, for this occurrence only.
		cache := make(map[int]FixedBox)
		fit := func(width int) FixedBox {
			width = max(1, width)
			if box, ok := cache[width]; ok {
				return box
			}
			box := b.layout(formula.tex, formula.display, width)
			cache[width] = box
			return box
		}
		preferred := 0
		if tableCell {
			preferred = fit(b.width).Width
		}
		items = append(items, flowItem{box: fit, preferred: preferred, block: formula.display})
		// A combining mark may share the marker's grapheme; retain its bytes.
		write(seq[size:])
	}
	flush()
	return items
}

func (b *markdownBoxes) expand(rendered string) (string, error) {
	var rows []string
	for _, row := range strings.Split(rendered, "\n") {
		expanded := false
		for index, r := range row {
			carrier, ok := b.carriers[r]
			if !ok {
				continue
			}
			if suffix := row[index+len(string(r)):]; strings.TrimSpace(ansi.Strip(suffix)) != "" {
				return "", fmt.Errorf("Markdown block carrier was not isolated from following content")
			}
			column := ansi.StringWidth(row[:index])
			prefix := ansi.Cut(row, 0, column)
			if carrier.listIndent > 0 && !strings.HasSuffix(ansi.Strip(prefix), carrier.decoration) {
				prefix += strings.Repeat(" ", carrier.listIndent)
			}
			// At tiny widths, drop decoration before sacrificing a two-cell
			// grapheme. Normal-sized panes retain all container prefixes.
			prefix = ansi.Truncate(prefix, max(0, b.width-2), "")
			column = ansi.StringWidth(prefix)
			// Glamour pads blocks with styled spaces. Those are available layout area,
			// not a suffix that should be repeated after the expanded content.
			available := max(1, b.width-column)
			for i, content := range carrier.layout(available) {
				decoration := prefix
				if i > 0 {
					decoration = continuationPrefix(prefix, carrier.decoration)
				}
				rows = append(rows, decoration+content)
			}
			expanded = true
			break
		}
		if !expanded {
			// Unsupported/non-content AST positions retain literal source rather
			// than exposing an internal token. Expanded box rows are never reparsed.
			if b.hasFormula(row) {
				row = wrapIndented(b.restore(row), b.width)
			}
			rows = append(rows, row)
		}
	}
	return strings.Join(rows, "\n"), nil
}
