package tui

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/color"
	"testing"
	"ttc/internal/render"
)

func TestWrappedRowsCarryStyleAndIgnoreControls(t *testing.T) {
	s := newTestScreen(t)
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
			_, style, _ := s.Get(x, y)
			fg, attrs := style.GetForeground(), style.GetAttributes()
			if fg == color.Default || attrs&tcell.AttrBold == 0 {
				t.Fatalf("lost style at %d,%d", x, y)
			}
		}
	}
	_, style, _ := s.Get(0, 2)
	attrs := style.GetAttributes()
	if attrs&tcell.AttrBold != 0 {
		t.Fatal("reset ignored")
	}
	putStyled(s, 0, 3, 4, "\x1b[2J\x1b]8;;https://example.org\a界é\x1b]8;;\a", tcell.StyleDefault)
	cluster, _, width := s.Get(0, 3)
	if cluster != "界" || width != 2 {
		t.Fatal(cluster, width)
	}
	cluster, _, _ = s.Get(2, 3)
	if cluster != "é" {
		t.Fatal(cluster)
	}
}

func TestMarkdownTableHeaderColorDiffersFromBody(t *testing.T) {
	s := newTestScreen(t)
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
			r, style, _ := s.Get(x, y)
			if r == "H" {
				header = style
			}
			if r == "b" {
				body = style
			}
		}
	}
	hfg, attrs := header.GetForeground(), header.GetAttributes()
	bfg := body.GetForeground()
	if hfg == bfg || hfg == color.Default || attrs&tcell.AttrBold == 0 {
		t.Fatal("table header is not distinct", header, body, text)
	}
}
