package graphics

import (
	"fmt"
	"image"
	"image/draw"
	"math"
)

const maxPlacementPixels = 16 << 20 // Bound temporary cell-aligned RGBA canvases.

// fitPlacement preserves aspect ratio in a cell-aligned transparent canvas.
// rasterScale is the source supersampling factor, 1 for ordinary images.
func fitPlacement(m image.Image, width, height, rasterScale int) (image.Image, error) {
	if m == nil || m.Bounds().Empty() || width < 1 || height < 1 || rasterScale < 1 {
		return nil, fmt.Errorf("image, positive canvas dimensions and raster scale are required")
	}
	if width > maxPlacementPixels/height {
		return nil, fmt.Errorf("image placement exceeds 16-megapixel canvas limit")
	}
	b := m.Bounds()
	scale := min(float64(width)/float64(b.Dx()), float64(height)/float64(b.Dy()), 1/float64(rasterScale))
	w := min(width, max(1, int(math.Round(float64(b.Dx())*scale))))
	h := min(height, max(1, int(math.Round(float64(b.Dy())*scale))))
	pixels, err := downsample(m, w, h)
	if err != nil {
		return nil, err
	}
	if w == width && h == height {
		return pixels, nil
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	// Left alignment keeps inline formulas attached to adjacent text; spare
	// vertical space centers the glyphs instead of stretching them.
	draw.Draw(out, image.Rect(0, (height-h)/2, w, (height-h)/2+h), pixels, pixels.Bounds().Min, draw.Src)
	return out, nil
}

// downsample averages exact source-pixel coverage in premultiplied sRGB and
// alpha. It never enlarges an axis. Transparent colors contribute no fringe;
// an unchanged image is returned directly without allocation or pixel reads.
func downsample(m image.Image, width, height int) (image.Image, error) {
	if m == nil || width < 1 || height < 1 {
		return nil, fmt.Errorf("image and positive target dimensions are required")
	}
	b := m.Bounds()
	if b.Empty() {
		return nil, fmt.Errorf("image dimensions must be positive")
	}
	width, height = min(width, b.Dx()), min(height, b.Dy())
	if width == b.Dx() && height == b.Dy() {
		return m, nil
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	sx, sy := float64(b.Dx())/float64(width), float64(b.Dy())/float64(height)
	// Standard decoded images support allocation-free color reads. The generic
	// path remains available for other image implementations.
	fast, _ := m.(image.RGBA64Image)
	for y := range height {
		top, bottom := float64(y)*sy, float64(y+1)*sy
		for x := range width {
			left, right := float64(x)*sx, float64(x+1)*sx
			var red, green, blue, alpha float64
			for j := int(top); j < min(b.Dy(), int(math.Ceil(bottom))); j++ {
				wy := min(bottom, float64(j+1)) - max(top, float64(j))
				for i := int(left); i < min(b.Dx(), int(math.Ceil(right))); i++ {
					weight := wy * (min(right, float64(i+1)) - max(left, float64(i)))
					var r, g, bl, a uint32
					if fast != nil {
						c := fast.RGBA64At(b.Min.X+i, b.Min.Y+j)
						r, g, bl, a = uint32(c.R), uint32(c.G), uint32(c.B), uint32(c.A)
					} else {
						r, g, bl, a = m.At(b.Min.X+i, b.Min.Y+j).RGBA()
					}
					red += float64(r) * weight
					green += float64(g) * weight
					blue += float64(bl) * weight
					alpha += float64(a) * weight
				}
			}
			area := (right - left) * (bottom - top) * 257
			p := out.Pix[out.PixOffset(x, y):]
			p[0], p[1], p[2], p[3] = byte(math.Round(red/area)), byte(math.Round(green/area)), byte(math.Round(blue/area)), byte(math.Round(alpha/area))
		}
	}
	return out, nil
}
