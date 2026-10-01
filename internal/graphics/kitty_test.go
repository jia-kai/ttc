package graphics

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/kitty"
)

func TestKittyPlacementChunksReuseAndCleanup(t *testing.T) {
	var out bytes.Buffer
	g := New(&out, false)
	g.Begin()
	m := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	random := rand.New(rand.NewSource(1))
	for y := range 100 {
		for x := range 100 {
			m.Set(x, y, color.NRGBA{byte(random.Intn(256)), byte(random.Intn(256)), byte(random.Intn(256)), 255})
		}
	}
	rows, err := g.Place("test", m, 12, 4)
	if err != nil {
		t.Fatal(err)
	}
	wire := out.String()
	for _, want := range []string{"a=T", "q=2", "U=1", "c=12", "r=4", "m=1", "m=0"} {
		if !strings.Contains(wire, want) {
			t.Fatal("missing option", want)
		}
	}
	if len(rows) != 4 || strings.Count(rows[0], string(kitty.Placeholder)) != 12 {
		t.Fatal(rows)
	}
	if !strings.Contains(rows[3], string([]rune{kitty.Placeholder, kitty.Diacritic(3), kitty.Diacritic(0)})) {
		t.Fatal("missing explicit coordinates")
	}
	before := out.Len()
	g.Begin()
	same, err := g.Place("test", m, 12, 4)
	if err != nil {
		t.Fatal(err)
	}
	if out.Len() != before || same[0] != rows[0] {
		t.Fatal("asset retransmitted")
	}
	if err = g.End(); err != nil {
		t.Fatal(err)
	}
	g.Begin()
	if err = g.End(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "d=I") {
		t.Fatal("not freed")
	}
	g.Begin()
	again, err := g.Place("test", m, 12, 4)
	if err != nil || again[0] != rows[0] {
		t.Fatal("cached placeholders lost identity", err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	if len(g.images) != 0 {
		t.Fatal("owned images leaked")
	}
}
func TestKittyTmuxAndInvalidGrid(t *testing.T) {
	var out bytes.Buffer
	g := New(&out, true)
	g.Begin()
	m := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	if _, err := g.Place("tmux", m, 2, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\x1bPtmux;\x1b\x1b_G") {
		t.Fatal("passthrough missing")
	}
	if _, err := g.Place("bad", m, 298, 1); err == nil {
		t.Fatal("out-of-range diacritic")
	}
}
func TestGeometryAspectAndCellPixels(t *testing.T) {
	m := image.NewNRGBA(image.Rect(0, 0, 320, 200))
	c, r := Geometry(m, 8, 16, 48, 10)
	if c != 32 || r != 10 {
		t.Fatal(c, r)
	}
	c, r = Geometry(m, 8, 16, 20, 10)
	if c != 20 || r != 7 {
		t.Fatal(c, r)
	}
}
