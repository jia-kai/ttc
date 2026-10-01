package tui

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/render"
	"testing"
)

func TestWrappedRowsCarryStyleAndIgnoreControls(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(4, 4)
	rows := styledRows(ansi.Hardwrap("\x1b[1;31mabcd\x1b[0mxy", 2, true))
	for y, text := range rows {
		putStyled(s, 0, y, 2, text, tcell.StyleDefault)
	}
	for y := range 2 {
		for x := range 2 {
			_, _, style, _ := s.GetContent(x, y)
			fg, _, attrs := style.Decompose()
			if fg == tcell.ColorDefault || attrs&tcell.AttrBold == 0 {
				t.Fatalf("lost style at %d,%d", x, y)
			}
		}
	}
	_, _, style, _ := s.GetContent(0, 2)
	_, _, attrs := style.Decompose()
	if attrs&tcell.AttrBold != 0 {
		t.Fatal("reset ignored")
	}
	putStyled(s, 0, 3, 4, "\x1b[2J\x1b]8;;https://example.org\a界é\x1b]8;;\a", tcell.StyleDefault)
	r, comb, _, width := s.GetContent(0, 3)
	if r != '界' || width != 2 || len(comb) != 0 {
		t.Fatal(r, comb, width)
	}
	r, comb, _, _ = s.GetContent(2, 3)
	if r != 'e' || string(comb) != "́" {
		t.Fatal(r, comb)
	}
}

func TestMarkdownTableHeaderColorDiffersFromBody(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(50, 20)
	text, err := render.Terminal("| Header | Other |\n| ------ | ----- |\n| body   | value |\n", 50)
	if err != nil {
		t.Fatal(err)
	}
	rows := styledRows(text)
	for y, text := range rows {
		putStyled(s, 0, y, 50, text, tcell.StyleDefault)
	}
	var header, body tcell.Style
	for y := range 20 {
		for x := range 50 {
			r, _, style, _ := s.GetContent(x, y)
			if r == 'H' {
				header = style
			}
			if r == 'b' {
				body = style
			}
		}
	}
	hfg, _, attrs := header.Decompose()
	bfg, _, _ := body.Decompose()
	if hfg == bfg || hfg == tcell.ColorDefault || attrs&tcell.AttrBold == 0 {
		t.Fatal("table header is not distinct", header, body, text)
	}
}
