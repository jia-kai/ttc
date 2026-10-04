package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/render"
)

func TestInlineCodeUsesLightForegroundWithoutBackground(t *testing.T) {
	text, err := render.Terminal("An identifier: `job_abc`.", 80)
	if err != nil {
		t.Fatal(err)
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	if err = screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(80, 20)
	base := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor))
	found := false
	for y, row := range styledRows(text) {
		putStyled(screen, 0, y, 80, row, base)
		var plain strings.Builder
		for x := range 80 {
			r, _, _, _ := screen.GetContent(x, y)
			plain.WriteRune(r)
		}
		if x := strings.Index(plain.String(), "job_abc"); x >= 0 {
			_, _, style, _ := screen.GetContent(x, y)
			fg, bg, _ := style.Decompose()
			if fg != tcell.GetColor(render.BlueColor) || bg != tcell.GetColor(render.SurfaceColor) {
				t.Fatal("inline code lost palette/inherited surface", fg, bg)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("identifier missing")
	}
}
