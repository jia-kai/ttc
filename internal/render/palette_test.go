package render

import (
	"fmt"
	"math"
	"testing"
)

func TestPaletteTextContrast(t *testing.T) {
	luminance := func(hex string) float64 {
		var rgb [3]int
		if _, err := fmt.Sscanf(hex, "#%02x%02x%02x", &rgb[0], &rgb[1], &rgb[2]); err != nil {
			t.Fatal(err)
		}
		sum := 0.0
		for i, weight := range []float64{0.2126, 0.7152, 0.0722} {
			c := float64(rgb[i]) / 255
			if c <= 0.04045 {
				c /= 12.92
			} else {
				c = math.Pow((c+0.055)/1.055, 2.4)
			}
			sum += c * weight
		}
		return sum
	}
	for _, bg := range []string{SurfaceColor, HumanColor} {
		for _, fg := range []string{TextColor, MutedColor, BlueColor, CyanColor, LavenderColor, AmberColor} {
			ratio := (luminance(fg) + 0.05) / (luminance(bg) + 0.05)
			if ratio < 4.5 {
				t.Errorf("%s on %s contrast %.2f < 4.5", fg, bg, ratio)
			}
		}
	}
}
