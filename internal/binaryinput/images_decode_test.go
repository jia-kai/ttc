package binaryinput

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestDecodeImageReturnsValidatedPixels(t *testing.T) {
	pixels := image.NewRGBA(image.Rect(0, 0, 3, 2))
	pixels.Set(1, 1, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, pixels); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeImage(context.Background(), encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != pixels.Bounds() || color.RGBAModel.Convert(decoded.At(1, 1)) != pixels.RGBAAt(1, 1) {
		t.Fatalf("decoded image differs: %v, %v", decoded.Bounds(), decoded.At(1, 1))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecodeImage(ctx, encoded.Bytes()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled decode: %v", err)
	}
}
