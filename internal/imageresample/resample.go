// Package imageresample provides antialiased image scaling and translation.
package imageresample

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"

	"golang.org/x/image/draw"
	"golang.org/x/image/math/f64"
)

// Draw replaces dst with a scaled src and transparent pixels outside it.
// Source-local (0, 0), src.Bounds().Min, maps to destination coordinate
// (originX, originY). Scales are destination pixels per source pixel; destination
// coordinates are absolute, including for images with nonzero bounds.
//
// Filtering uses golang.org/x/image/draw: linear interpolation when enlarging
// and BiLinear's widened tent kernel for antialiased reduction on each axis.
// Premultiplied sRGB channels and alpha are filtered together, not in linear
// light. Source-edge weights are normalized; this is not exact area averaging
// and does not compute geometric partial-pixel coverage at image boundaries.
//
// Context and images must be non-nil, images nonempty, scales finite and
// positive, and origins finite. Source and destination must not share storage.
// Typed-nil source images are not supported. Unrepresentable affine transforms
// return an error. Images implementing image.RGBA64Image avoid per-sample
// color-interface allocations.
//
// Cancellation is checked before work and, for cancellable contexts, before
// each source sample. After cancellation further source reads stop, but the
// underlying filter finishes scanning its output before Draw returns ctx.Err().
// Destination contents are unspecified on cancellation. Background contexts
// preserve the library's optimized concrete-image paths.
func Draw(ctx context.Context, dst *image.RGBA, src image.Image, scaleX, scaleY, originX, originY float64) error {
	if ctx == nil || dst == nil || src == nil {
		return errors.New("imageresample: nil context or image")
	}
	if dst.Bounds().Empty() || src.Bounds().Empty() {
		return errors.New("imageresample: empty image")
	}
	if !finite(scaleX) || !finite(scaleY) || scaleX <= 0 || scaleY <= 0 || !finite(originX) || !finite(originY) {
		return errors.New("imageresample: scales must be finite and positive, and origins finite")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sr, dr := src.Bounds(), dst.Bounds()
	m := f64.Aff3{scaleX, 0, originX - scaleX*float64(sr.Min.X), 0, scaleY, originY - scaleY*float64(sr.Min.Y)}
	// The upstream transform uses int32 kernel indices and an inverted matrix.
	if !finite(m[2]) || !finite(m[5]) || !finite(scaleX*scaleY) || scaleX*scaleY == 0 || 1/scaleX >= math.MaxInt32/2 || 1/scaleY >= math.MaxInt32/2 {
		return errors.New("imageresample: unrepresentable affine transform")
	}
	if ctx.Done() != nil {
		fast, _ := src.(image.RGBA64Image)
		src = &contextImage{Image: src, fast: fast, ctx: ctx}
	}
	// Bound BiLinear.Scale's float64 RGBA intermediate to 32 MiB. Tall,
	// narrow sources otherwise allocate far more scratch space than output.
	if dr.Dx() <= (1<<20)/sr.Dy() && originX == float64(dr.Min.X) && originY == float64(dr.Min.Y) && math.Abs(scaleX*float64(sr.Dx())-float64(dr.Dx())) < 1e-9 && math.Abs(scaleY*float64(sr.Dy())-float64(dr.Dy())) < 1e-9 {
		// The separable scaler avoids repeating 2-D kernel work for full resizes.
		draw.BiLinear.Scale(dst, dr, src, sr, draw.Src, nil)
	} else {
		// Transform touches only its affected region. Clear the remainder while
		// respecting padding and neighboring pixels of destination subimages.
		for y := dr.Min.Y; y < dr.Max.Y; y++ {
			i := dst.PixOffset(dr.Min.X, y)
			clear(dst.Pix[i : i+dr.Dx()*4])
		}
		draw.BiLinear.Transform(dst, m, src, sr, draw.Src, nil)
	}
	return ctx.Err()
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type contextImage struct {
	image.Image
	fast image.RGBA64Image
	ctx  context.Context
}

func (s *contextImage) RGBA64At(x, y int) color.RGBA64 {
	if s.ctx.Err() != nil {
		return color.RGBA64{}
	}
	if s.fast != nil {
		return s.fast.RGBA64At(x, y)
	}
	r, g, b, a := s.Image.At(x, y).RGBA()
	return color.RGBA64{R: uint16(r), G: uint16(g), B: uint16(b), A: uint16(a)}
}
