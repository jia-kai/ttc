package graphics

import (
	"image"
	"image/color"
	"testing"
)

func TestDownsampleCoverageAndTransparency(t *testing.T) {
	// One white stroke in each 3-pixel span must become one-third coverage,
	// regardless of hidden magenta in the surrounding transparent pixels.
	m := image.NewNRGBA(image.Rect(4, 8, 10, 14))
	for y := 8; y < 14; y++ {
		for x := 4; x < 10; x++ {
			c := color.NRGBA{R: 255, B: 255}
			if (x-4)%3 == 1 {
				c = color.NRGBA{255, 255, 255, 255}
			}
			m.SetNRGBA(x, y, c)
		}
	}
	out, err := downsample(m, 2, 2)
	if err != nil || out.Bounds() != image.Rect(0, 0, 2, 2) {
		t.Fatal(out, err)
	}
	for y := range 2 {
		for x := range 2 {
			if got := color.RGBAModel.Convert(out.At(x, y)); got != (color.RGBA{85, 85, 85, 85}) {
				t.Fatal("incorrect alpha coverage or colored halo", got)
			}
		}
	}
	if m.NRGBAAt(4, 8) != (color.NRGBA{R: 255, B: 255}) {
		t.Fatal("source modified")
	}
}

func TestDownsampleFractionalRatiosAndAxisBounds(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 3, 1))
	m.SetRGBA(0, 0, color.RGBA{A: 255})
	m.SetRGBA(1, 0, color.RGBA{255, 255, 255, 255})
	m.SetRGBA(2, 0, color.RGBA{A: 255})
	out, err := downsample(m, 2, 4)
	if err != nil || out.Bounds() != image.Rect(0, 0, 2, 1) {
		t.Fatal(out, err)
	}
	for x := range 2 {
		if got := color.RGBAModel.Convert(out.At(x, 0)); got != (color.RGBA{85, 85, 85, 255}) {
			t.Fatal("fractional area not averaged", got)
		}
	}
	unchanged, err := downsample(m, 3, 10)
	if err != nil || unchanged != m {
		t.Fatal("unnecessary allocation", err)
	}
	for _, tc := range []struct {
		m image.Image
		w int
		h int
	}{{nil, 1, 1}, {m, 0, 1}, {m, 1, -1}, {image.NewRGBA(image.Rect(0, 0, 0, 1)), 1, 1}} {
		if _, err := downsample(tc.m, tc.w, tc.h); err == nil {
			t.Fatal("invalid image/geometry accepted")
		}
	}
}

func TestDownsampleCheckerboard(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := range 100 {
		for x := range 100 {
			c := color.RGBA{A: 255}
			if (x+y)%2 == 0 {
				c = color.RGBA{255, 255, 255, 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	out, err := downsample(m, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 10 {
		for x := range 10 {
			if got := color.RGBAModel.Convert(out.At(x, y)); got != (color.RGBA{128, 128, 128, 255}) {
				t.Fatal("aliasing instead of area filtering", got)
			}
		}
	}
}

func BenchmarkDownsampleFormula(b *testing.B) {
	m := image.NewNRGBA(image.Rect(0, 0, 1800, 144))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := downsample(m, 600, 48); err != nil {
			b.Fatal(err)
		}
	}
}
