package graphics

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
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
	rows, err := g.Place("test", m, 12, 4, 1)
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
	same, err := g.Place("test", m, 12, 4, 1)
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
	again, err := g.Place("test", m, 12, 4, 1)
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

func TestKittyFormulaKeepsAspectRatioInsideRoundedCells(t *testing.T) {
	for _, tc := range []struct{ width, height, cw, ch, cols, rows int }{
		{40, 12, 8, 16, 5, 1}, {40, 40, 8, 16, 5, 3},
		{19, 17, 8, 16, 3, 2}, {36, 22, 11, 23, 4, 1},
		{200, 20, 8, 16, 5, 1}, {40, 200, 8, 16, 5, 3},
	} {
		var output bytes.Buffer
		g := New(&output, false)
		g.CellWidth, g.CellHeight = tc.cw, tc.ch
		g.Begin()
		m := image.NewRGBA(image.Rect(0, 0, tc.width*3, tc.height*3))
		for y := range m.Bounds().Dy() {
			for x := range m.Bounds().Dx() {
				m.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			}
		}
		if _, err := g.Place("formula", m, tc.cols, tc.rows, 3); err != nil {
			t.Fatal(err)
		}
		pixels := uploadedPNG(t, output.String())
		bounds := image.Rectangle{}
		for y := range pixels.Bounds().Dy() {
			for x := range pixels.Bounds().Dx() {
				_, _, _, a := pixels.At(x, y).RGBA()
				if a != 0 {
					bounds = bounds.Union(image.Rect(x, y, x+1, y+1))
				}
			}
		}
		// Both axes must share one scale, within integer-pixel rounding.
		sx, sy := float64(bounds.Dx())/float64(tc.width), float64(bounds.Dy())/float64(tc.height)
		if sx > 1 || sy > 1 || math.Abs(sx-sy) > 1/float64(tc.width)+1/float64(tc.height) {
			t.Fatal("glyphs stretched or enlarged", tc, bounds, sx, sy)
		}
		if tc.width == 40 && tc.height <= 40 && bounds.Size() != image.Pt(tc.width, tc.height) {
			t.Fatal("rounded cell changed logical dimensions", tc, bounds)
		}
		if pixels.Bounds() != image.Rect(0, 0, tc.cols*tc.cw, tc.rows*tc.ch) {
			t.Fatal("canvas does not match placement", pixels.Bounds())
		}
	}
}

func TestKittyRejectsOversizedPlacementBeforeAllocation(t *testing.T) {
	var output bytes.Buffer
	g := New(&output, false)
	g.Begin()
	g.CellWidth, g.CellHeight = int(^uint(0)>>1), 16
	if _, err := g.Place("tiny", image.NewRGBA(image.Rect(0, 0, 1, 1)), 2, 1, 1); err == nil || output.Len() != 0 {
		t.Fatal("overflowing placement accepted", err)
	}
	g.CellWidth, g.CellHeight = 100000, 100000
	if _, err := g.Place("tiny", image.NewRGBA(image.Rect(0, 0, 1, 1)), 1, 1, 1); err == nil || output.Len() != 0 {
		t.Fatal("oversized placement accepted", err)
	}
}

type countedImage struct {
	image.Image
	reads int
}

func (m *countedImage) At(x, y int) color.Color {
	m.reads++
	return m.Image.At(x, y)
}

func uploadedPNG(t *testing.T, wire string) image.Image {
	t.Helper()
	var encoded strings.Builder
	for _, chunk := range strings.Split(wire, "\x1b_G")[1:] {
		body, _, ok := strings.Cut(chunk, "\x1b\\")
		if !ok {
			t.Fatal("incomplete graphics command")
		}
		_, data, ok := strings.Cut(body, ";")
		if ok {
			encoded.WriteString(data)
		}
	}
	data, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil {
		t.Fatal(err)
	}
	m, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestKittyFiltersUploadOncePerPixelGeometry(t *testing.T) {
	var output bytes.Buffer
	g := New(&output, false)
	source := image.NewNRGBA(image.Rect(0, 0, 24, 48))
	for y := range 48 {
		for x := range 24 {
			c := color.NRGBA{R: 255, B: 255} // invisible color must not bleed
			if x%3 == 1 {
				c = color.NRGBA{255, 255, 255, 255}
			}
			source.SetNRGBA(x, y, c)
		}
	}
	m := &countedImage{Image: source}
	g.Begin()
	before, err := g.Rows("formula", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := g.Place("formula", m, 1, 1, 3)
	if err != nil || rows[0] != before[0] {
		t.Fatal("layout/upload identities differ", err)
	}
	decoded := uploadedPNG(t, output.String())
	if decoded.Bounds() != image.Rect(0, 0, 8, 16) {
		t.Fatal("upload retained supersampled dimensions", decoded.Bounds())
	}
	for y := range 16 {
		for x := range 8 {
			if got := color.NRGBAModel.Convert(decoded.At(x, y)); got != (color.NRGBA{255, 255, 255, 85}) {
				t.Fatal("wire PNG lost area coverage", got)
			}
		}
	}
	reads, size := m.reads, output.Len()
	g.Begin()
	if _, err = g.Place("formula", m, 1, 1, 3); err != nil {
		t.Fatal(err)
	}
	if m.reads != reads || output.Len() != size {
		t.Fatal("redraw resampled/retransmitted cached image")
	}
	// Font width can change while the terminal grid stays unchanged.
	g.CellWidth = 12
	output.Reset()
	g.Begin()
	after, err := g.Rows("formula", 1, 1)
	if err != nil || after[0] == before[0] {
		t.Fatal("cell pixel size missing from identity", err)
	}
	rows, err = g.Place("formula", m, 1, 1, 3)
	if err != nil || rows[0] != after[0] {
		t.Fatal("resized layout/upload identities differ", err)
	}
	if got := uploadedPNG(t, output.String()).Bounds(); got != image.Rect(0, 0, 12, 16) {
		t.Fatal("stale upload after font resize", got)
	}
	if err = g.End(); err != nil || len(g.images) != 1 {
		t.Fatal("old geometry retained", err)
	}
	if !strings.Contains(output.String(), "d=I") {
		t.Fatal("old geometry was not freed")
	}
}
func TestKittyTmuxAndInvalidGrid(t *testing.T) {
	var out bytes.Buffer
	g := New(&out, true)
	g.Begin()
	m := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	if _, err := g.Place("tmux", m, 2, 1, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\x1bPtmux;\x1b\x1b_G") {
		t.Fatal("passthrough missing")
	}
	if _, err := g.Place("bad", m, 298, 1, 1); err == nil {
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
