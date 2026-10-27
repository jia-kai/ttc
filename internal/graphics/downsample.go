package graphics

import (
	"context"
	"fmt"
	"image"
	"image/draw"
	"math"

	"ttc/internal/imageresample"
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

// downsample uses the shared antialiased bilinear filter. It never enlarges
// an axis and returns unchanged images directly without allocation or pixel reads.
func downsample(m image.Image, width, height int) (image.Image, error) {
	if m == nil || m.Bounds().Empty() || width < 1 || height < 1 {
		return nil, fmt.Errorf("image and positive target dimensions are required")
	}
	b := m.Bounds()
	width, height = min(width, b.Dx()), min(height, b.Dy())
	if width == b.Dx() && height == b.Dy() {
		return m, nil
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	if err := imageresample.Draw(context.Background(), out, m, float64(width)/float64(b.Dx()), float64(height)/float64(b.Dy()), 0, 0); err != nil {
		return nil, err
	}
	return out, nil
}
