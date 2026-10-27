package imageresample

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

func TestDrawAntialiasesFractionalReduction(t *testing.T) {
	src := image.NewRGBA(image.Rect(4, 6, 132, 134))
	for y := 6; y < 134; y++ {
		for x := 4; x < 132; x++ {
			v := uint8(0)
			if (x+y)%2 == 0 {
				v = 255
			}
			src.SetRGBA(x, y, color.RGBA{v, v, v, 255})
		}
	}
	for _, origin := range []float64{0, 0.375, -0.125} {
		dst := image.NewRGBA(image.Rect(0, 0, 32, 32))
		if err := Draw(context.Background(), dst, src, 0.25, 0.25, origin, -origin); err != nil {
			t.Fatal(err)
		}
		for y := 2; y < 30; y++ {
			for x := 2; x < 30; x++ {
				c := dst.RGBAAt(x, y)
				if c.R < 126 || c.R > 129 || c.G != c.R || c.B != c.R || c.A != 255 {
					t.Fatalf("origin %v pixel %d,%d aliases: %v", origin, x, y, c)
				}
			}
		}
	}
}

func TestDrawLinearUpsamplingAndAnisotropy(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		src.SetRGBA(0, y, color.RGBA{R: 255, A: 255})
		src.SetRGBA(1, y, color.RGBA{B: 255, A: 255})
	}
	for _, sy := range []float64{0.5, 2} {
		// A larger destination forces the affine path instead of full resize.
		dst := image.NewRGBA(image.Rect(0, 0, 5, int(2*sy)))
		if err := Draw(context.Background(), dst, src, 2, sy, 0, 0); err != nil {
			t.Fatal(err)
		}
		for i, want := range []color.RGBA{{R: 255, A: 255}, {R: 191, B: 64, A: 255}, {R: 64, B: 191, A: 255}, {B: 255, A: 255}, {}} {
			if got := dst.RGBAAt(i, 0); got != want {
				t.Errorf("sy=%v x=%d: got %v want %v", sy, i, got, want)
			}
		}
	}
}

func TestDrawPremultipliedAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{B: 255}) // Hidden blue must not bleed.
	for _, source := range []image.Image{src, imageOnly{src}} {
		for _, cancellable := range []bool{false, true} {
			ctx := context.Background()
			if cancellable {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			dst := image.NewRGBA(image.Rect(0, 0, 1, 1))
			if err := Draw(ctx, dst, source, 0.5, 1, 0, 0); err != nil {
				t.Fatal(err)
			}
			if got := dst.RGBAAt(0, 0); got.R < 127 || got.R > 128 || got.A != got.R || got.G != 0 || got.B != 0 {
				t.Fatal(got)
			}
		}
	}
}

type imageOnly struct{ image.Image }

func TestDrawOffsetBoundsAndTransparentOutside(t *testing.T) {
	src := image.NewRGBA(image.Rect(-7, 11, -3, 13))
	for y := 11; y < 13; y++ {
		for x := -7; x < -3; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8((x + 8) * 40), A: 255})
		}
	}
	parent := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for i := range parent.Pix {
		parent.Pix[i] = 255
	}
	dst := parent.SubImage(image.Rect(2, 3, 7, 7)).(*image.RGBA)
	if err := Draw(context.Background(), dst, src, 1, 1, 3, 4); err != nil {
		t.Fatal(err)
	}
	if got := dst.RGBAAt(3, 4); got != (color.RGBA{R: 40, A: 255}) {
		t.Fatal(got)
	}
	if got := dst.RGBAAt(6, 5); got != (color.RGBA{R: 160, A: 255}) {
		t.Fatal(got)
	}
	if got := dst.RGBAAt(2, 3); got != (color.RGBA{}) {
		t.Fatal("outside source not cleared:", got)
	}
	if got := parent.RGBAAt(1, 4); got != (color.RGBA{255, 255, 255, 255}) {
		t.Fatal("wrote outside destination", got)
	}
}

type observedImage struct {
	*image.RGBA
	calls, cancelAt int
	cancel          context.CancelFunc
}

func (s *observedImage) RGBA64At(x, y int) color.RGBA64 {
	s.calls++
	if s.calls == s.cancelAt && s.cancel != nil {
		s.cancel()
	}
	return s.RGBA.RGBA64At(x, y)
}

func TestDrawCancellation(t *testing.T) {
	for _, affine := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		src := &observedImage{RGBA: image.NewRGBA(image.Rect(0, 0, 32, 100)), cancelAt: 3, cancel: cancel}
		origin := 0.0
		if affine {
			origin = 0.125
		}
		err := Draw(ctx, image.NewRGBA(image.Rect(0, 0, 1, 1)), src, 1.0/32, 0.01, origin, 0)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("affine=%v: got %v", affine, err)
		}
		if src.calls != 3 {
			t.Fatalf("read %d samples after cancellation", src.calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &observedImage{RGBA: image.NewRGBA(image.Rect(0, 0, 2, 2))}
	if err := Draw(ctx, image.NewRGBA(src.Bounds()), src, 1, 1, 0, 0); !errors.Is(err, context.Canceled) || src.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, src.calls)
	}
}

func TestDrawClipsOffscreenSource(t *testing.T) {
	src := &observedImage{RGBA: image.NewRGBA(image.Rect(0, 0, 1000, 1000))}
	if err := Draw(context.Background(), image.NewRGBA(image.Rect(0, 0, 1, 1)), src, 0.5, 0.5, -200, -200); err != nil {
		t.Fatal(err)
	}
	if src.calls == 0 || src.calls > 16 {
		t.Fatalf("read %d samples for a 1-pixel viewport", src.calls)
	}
	src.calls = 0
	if err := Draw(context.Background(), image.NewRGBA(image.Rect(0, 0, 1, 1)), src, 0.5, 0.5, 2, 2); err != nil {
		t.Fatal(err)
	}
	if src.calls != 0 {
		t.Fatalf("read %d offscreen samples", src.calls)
	}
}

func TestDrawInvalidArguments(t *testing.T) {
	valid := image.NewRGBA(image.Rect(0, 0, 1, 1))
	tests := []struct {
		ctx context.Context
		dst *image.RGBA
		src image.Image
	}{
		{nil, valid, valid}, {context.Background(), nil, valid}, {context.Background(), valid, nil},
		{context.Background(), &image.RGBA{}, valid}, {context.Background(), valid, &image.RGBA{}},
	}
	for _, tc := range tests {
		if Draw(tc.ctx, tc.dst, tc.src, 1, 1, 0, 0) == nil {
			t.Fatal("accepted invalid image/context")
		}
	}
	for axis := 0; axis < 4; axis++ {
		for _, v := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1)} {
			if axis >= 2 && !math.IsNaN(v) && !math.IsInf(v, 0) {
				continue
			}
			args := [4]float64{1, 1, 0, 0}
			args[axis] = v
			if Draw(context.Background(), valid, valid, args[0], args[1], args[2], args[3]) == nil {
				t.Errorf("accepted axis %d value %v", axis, v)
			}
		}
	}
	for _, v := range []float64{math.SmallestNonzeroFloat64, math.MaxFloat64} {
		if Draw(context.Background(), valid, valid, v, v, 0, 0) == nil {
			t.Errorf("accepted unrepresentable determinant %v", v)
		}
	}
}

func TestDrawAllocationBounds(t *testing.T) {
	for _, src := range []image.Image{image.NewRGBA(image.Rect(0, 0, 32, 32)), image.NewNRGBA(image.Rect(0, 0, 32, 32)), image.NewRGBA64(image.Rect(0, 0, 32, 32))} {
		for _, origin := range []float64{0, 0.125} {
			dst := image.NewRGBA(image.Rect(0, 0, 8, 8))
			allocs := testing.AllocsPerRun(10, func() {
				if err := Draw(context.Background(), dst, src, 0.25, 0.25, origin, 0); err != nil {
					panic(err)
				}
			})
			if allocs > 8 {
				t.Errorf("%T origin %v: %g allocations", src, origin, allocs)
			}
		}
	}
}

func BenchmarkDraw(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 2191, 967))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 17)
	}
	for _, tc := range []struct {
		name           string
		sx, sy, ox, oy float64
	}{
		{"Resize2191x967To512x225", 512.0 / 2191, 225.0 / 967, 0, 0},
		{"FractionalPreview", 0.23, 0.23, -12.375, -3.125},
		{"Upsample2xViewport", 2, 2, -100.375, -30.125},
	} {
		b.Run(tc.name, func(b *testing.B) {
			dst := image.NewRGBA(image.Rect(0, 0, 512, 225))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := Draw(context.Background(), dst, src, tc.sx, tc.sy, tc.ox, tc.oy); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkThumbnailContext(b *testing.B) {
	src := image.NewNRGBA(image.Rect(0, 0, 2191, 967))
	for i := range src.Pix {
		src.Pix[i] = uint8(i * 17)
	}
	for _, cancellable := range []bool{false, true} {
		name := "Background"
		if cancellable {
			name = "WithCancel"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			if cancellable {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			dst := image.NewRGBA(image.Rect(0, 0, 512, 225))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := Draw(ctx, dst, src, 512.0/2191, 225.0/967, 0, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPreview1200x600(b *testing.B) {
	src := image.NewRGBA(image.Rect(0, 0, 2191, 967))
	for y := range 967 {
		for x := range 2191 {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / 2191), G: uint8(y * 255 / 967), B: uint8((x + y) % 251), A: 255})
		}
	}
	for _, mode := range []string{"Draw", "PNGEncode", "DrawAndPNGEncode"} {
		b.Run(mode, func(b *testing.B) {
			dst := image.NewRGBA(image.Rect(0, 0, 1200, 600))
			var out bytes.Buffer
			render := func() {
				if err := Draw(context.Background(), dst, src, 0.54, 0.54, 8.375, 38.125); err != nil {
					b.Fatal(err)
				}
			}
			render()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if mode != "PNGEncode" {
					render()
				}
				if mode != "Draw" {
					out.Reset()
					if err := png.Encode(&out, dst); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
