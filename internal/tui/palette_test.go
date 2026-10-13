package tui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v3"
	"github.com/gdamore/tcell/v3/color"
	"ttc/internal/render"
)

func TestInlineCodeUsesLightForegroundWithoutBackground(t *testing.T) {
	text, err := render.Terminal("An identifier: `job_abc`.", 80)
	if err != nil {
		t.Fatal(err)
	}
	screen := newTestScreen(t)
	if err = screen.Init(); err != nil {
		t.Fatal(err)
	}
	defer screen.Fini()
	screen.SetSize(80, 20)
	base := tcell.StyleDefault.Background(color.GetColor(render.SurfaceColor))
	found := false
	for y, row := range styledRows(text) {
		putStyled(screen, 0, y, 80, row, base)
		var plain strings.Builder
		for x := range 80 {
			r, _, _ := screen.Get(x, y)
			plain.WriteString(r)
		}
		if x := strings.Index(plain.String(), "job_abc"); x >= 0 {
			_, style, _ := screen.Get(x, y)
			fg, bg := style.GetForeground(), style.GetBackground()
			if fg != color.GetColor(render.BlueColor) || bg != color.GetColor(render.SurfaceColor) {
				t.Fatal("inline code lost palette/inherited surface", fg, bg)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("identifier missing")
	}
}
