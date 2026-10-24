package render

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

const maxFormulaBytes = 4096

var kindFormula = ast.NewNodeKind("Formula")

type formulaNode struct {
	ast.BaseInline
	tex        string
	display    bool
	start, end int
}

func (n *formulaNode) Kind() ast.NodeKind            { return kindFormula }
func (n *formulaNode) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }
func (n *formulaNode) Text(source []byte) []byte     { return source[n.start:n.end] }

type formulaParser struct{}

func (formulaParser) Trigger() []byte { return []byte{'$', '\\'} }

// Goldmark's reader contains only the current block's logical segments: quote
// markers and list indentation have already been consumed by block parsers.
func (formulaParser) Parse(_ ast.Node, reader text.Reader, _ parser.Context) ast.Node {
	line, _ := reader.PeekLine()
	open, close, display := "", "", false
	switch {
	case bytes.HasPrefix(line, []byte("$$")):
		open, close, display = "$$", "$$", true
	case bytes.HasPrefix(line, []byte(`\[`)):
		open, close, display = `\[`, `\]`, true
	case bytes.HasPrefix(line, []byte(`\(`)):
		open, close = `\(`, `\)`
	case len(line) > 1 && line[0] == '$':
		r, _ := utf8.DecodeRune(line[1:])
		if !unicode.IsSpace(r) {
			open, close = "$", "$"
		}
	}
	if open == "" {
		return nil
	}
	// A delimiter-only logical line belongs exclusively to the display block
	// parser. If its closer was not valid in this container, keep it literal
	// instead of letting inline parsing consume unrelated later Markdown.
	if display && reader.LineOffset() == 0 && bytes.Equal(bytes.TrimSpace(line), []byte(open)) {
		return nil
	}
	savedLine, savedPosition := reader.Position()
	reader.Advance(len(open))
	var formula strings.Builder
	for {
		line, _ = reader.PeekLine()
		if len(line) == 0 {
			break
		}
		part := line[:min(len(line), maxFormulaBytes+len(close)+1-formula.Len())]
		if end := mathClosingDelimiter(part, close); end >= 0 && formula.Len()+end <= maxFormulaBytes {
			formula.Write(line[:end])
			value := formula.String()
			last, _ := utf8.DecodeLastRuneInString(value)
			if !display && (value == "" || unicode.IsSpace(last) || strings.Contains(value, "\n")) {
				break
			}
			reader.Advance(end + len(close))
			_, position := reader.Position()
			if position.Start-savedPosition.Start > maxFormulaBytes+4 {
				break
			}
			return &formulaNode{tex: value, display: display, start: savedPosition.Start, end: position.Start}
		}
		if !display || formula.Len()+len(line) > maxFormulaBytes {
			break
		}
		formula.Write(line)
		reader.AdvanceLine()
	}
	reader.SetPosition(savedLine, savedPosition)
	return nil
}

var kindDisplayFormula = ast.NewNodeKind("DisplayFormula")

type displayFormula struct {
	ast.BaseBlock
	open, close string
	start, end  int
	closed      bool
}

func (n *displayFormula) Kind() ast.NodeKind            { return kindDisplayFormula }
func (n *displayFormula) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }
func (n *displayFormula) formula(source []byte) string {
	var value strings.Builder
	value.WriteByte('\n')
	for i := 0; i < n.Lines().Len(); i++ {
		segment := n.Lines().At(i)
		value.Write(segment.Value(source))
	}
	return value.String()
}

type displayFormulaParser struct{}

func (displayFormulaParser) Trigger() []byte { return []byte{'$', '\\'} }
func (displayFormulaParser) Open(parent ast.Node, reader text.Reader, context parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	position := context.BlockOffset()
	if position < 0 {
		return nil, parser.NoChildren
	}
	open := strings.TrimSpace(string(line[position:]))
	close := ""
	switch open {
	case "$$":
		close = "$$"
	case `\[`:
		close = `\]`
	default:
		return nil, parser.NoChildren
	}
	// Do not turn an unfinished opening delimiter into a fence that swallows
	// the rest of a streamed document. Continue validates logical closing lines.
	limit := min(len(reader.Source()), segment.Start+position+maxFormulaBytes+4)
	if segment.Stop >= limit {
		return nil, parser.NoChildren
	}
	remaining := reader.Source()[segment.Stop:limit]
	if !displayCloserAhead(parent, remaining, close) {
		return nil, parser.NoChildren
	}
	return &displayFormula{open: open, close: close, start: segment.Start + position}, parser.NoChildren
}

// Validate a closing line with Goldmark's own container continuations. A private
// reader preserves visual tab columns and virtual padding; cloned ancestors and
// context keep lookahead independent of the live parse. The proposed display
// child makes list continuations observe a nonempty item, as the real parse will.
func displayCloserAhead(parent ast.Node, source []byte, close string) bool {
	var ancestors []ast.Node
	for n := parent; n != nil; n = n.Parent() {
		ancestors = append(ancestors, n)
	}
	var containers []parser.Block
	var enclosing ast.Node = ast.NewDocument()
	for i := len(ancestors) - 1; i >= 0; i-- {
		var node ast.Node
		var continuation parser.BlockParser
		switch n := ancestors[i].(type) {
		case *ast.Document:
			continue
		case *ast.Blockquote:
			node, continuation = ast.NewBlockquote(), parser.NewBlockquoteParser()
		case *ast.List:
			list := ast.NewList(n.Marker)
			list.Start = n.Start
			node, continuation = list, parser.NewListParser()
		case *ast.ListItem:
			node, continuation = ast.NewListItem(n.Offset), parser.NewListItemParser()
		case *extast.DefinitionList:
			node, continuation = extast.NewDefinitionList(n.Offset, nil), extension.NewDefinitionListParser()
		case *extast.DefinitionDescription:
			node, continuation = extast.NewDefinitionDescription(), extension.NewDefinitionDescriptionParser()
		default:
			return false
		}
		enclosing.AppendChild(enclosing, node)
		enclosing = node
		containers = append(containers, parser.Block{Node: node, Parser: continuation})
	}
	content := ast.NewTextBlock()
	enclosing.AppendChild(enclosing, content)
	context := parser.NewContext()
	context.SetOpenedBlocks(append(append([]parser.Block(nil), containers...), parser.Block{Node: content, Parser: displayFormulaParser{}}))
	reader := text.NewReader(source)
	for {
		if line, _ := reader.PeekLine(); line == nil {
			return false
		}
		for _, container := range containers {
			if container.Parser.Continue(container.Node, reader, context)&parser.Continue == 0 {
				return false
			}
		}
		line, _ := reader.PeekLine()
		if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte(close)) {
			return true
		}
		reader.AdvanceLine()
	}
}
func (displayFormulaParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	n := node.(*displayFormula)
	line, segment := reader.PeekLine()
	left := len(line) - len(bytes.TrimLeft(line, " \t"))
	if bytes.HasPrefix(line[left:], []byte(n.close)) {
		n.closed = true
		n.end = segment.Start + left + len(n.close)
		reader.Advance(left + len(n.close))
		return parser.Close
	}
	n.Lines().Append(segment)
	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}
func (displayFormulaParser) Close(ast.Node, text.Reader, parser.Context) {}
func (displayFormulaParser) CanInterruptParagraph() bool                 { return true }
func (displayFormulaParser) CanAcceptIndentedLine() bool                 { return false }
