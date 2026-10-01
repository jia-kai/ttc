package tui

import (
	"image/color"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
)

// styledRows makes each viewport row self-contained, preserving SGR across wraps.
func styledRows(text string) []string {
	parser := ansi.NewParser()
	var state byte
	var pen uv.Style
	var row strings.Builder
	var rows []string
	for len(text) > 0 {
		seq, _, n, next := ansi.DecodeSequence(text, state, parser)
		text, state = text[n:], next
		if seq == "\n" {
			rows = append(rows, row.String())
			row.Reset()
			row.WriteString(pen.String())
			continue
		}
		if parser.Command() == 'm' {
			uv.ReadStyle(parser.Params(), &pen)
		}
		row.WriteString(seq)
	}
	return append(rows, row.String())
}

// putStyled draws renderer-generated SGR as cells. Other escape commands are ignored,
// so even hyperlinks never execute terminal controls or launch external programs.
func putStyled(screen tcell.Screen, x, y, width int, text string, base tcell.Style) {
	previous := -1
	parser := ansi.NewParser()
	var state byte
	var pen uv.Style
	for len(text) > 0 && width > 0 {
		seq, cells, n, next := ansi.DecodeSequence(text, state, parser)
		text, state = text[n:], next
		if cells == 0 {
			if parser.Command() == 'm' {
				uv.ReadStyle(parser.Params(), &pen)
			} else if runes := []rune(seq); previous >= 0 && len(runes) > 0 && unicode.IsMark(runes[0]) {
				main, combining, style, _ := screen.GetContent(previous, y)
				screen.SetContent(previous, y, main, append(combining, runes...), style)
			}
			continue
		}
		if cells > width {
			break
		}
		style := base.Bold(pen.Attrs&uv.AttrBold != 0).Italic(pen.Attrs&uv.AttrItalic != 0).StrikeThrough(pen.Attrs&uv.AttrStrikethrough != 0).Underline(pen.Underline != 0)
		if pen.Fg != nil {
			style = style.Foreground(terminalColor(pen.Fg))
		}
		if pen.Bg != nil {
			style = style.Background(terminalColor(pen.Bg))
		}
		runes := []rune(seq)
		screen.SetContent(x, y, runes[0], runes[1:], style)
		previous = x
		x, width = x+cells, width-cells
	}
}

func terminalColor(c color.Color) tcell.Color {
	r, g, b, _ := c.RGBA()
	return tcell.NewRGBColor(int32(r>>8), int32(g>>8), int32(b>>8))
}
