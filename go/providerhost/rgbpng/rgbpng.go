// Package rgbpng writes an engine's raw pixels as a PNG with Go's standard
// library encoder, so a provider whose engine returns pixels delivers an
// image every reader opens and no engine-side encoder touches the result
// (inference CONTRACT.md INF-IO1).
package rgbpng

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
)

// PNGMediaType is the delivery's media type.
const PNGMediaType = "image/png"

// MaxSide bounds each side of an image this package writes.
const MaxSide = 8192

// ErrPixels wraps every refusal of a size or a pixel buffer.
var ErrPixels = errors.New("rgbpng: pixels refused")

// Encode writes width × height RGB24 pixels, rows top to bottom with no
// padding, as an 8-bit truecolour PNG with no alpha channel.
func Encode(width, height int, rgb []byte) ([]byte, error) {
	if width < 1 || height < 1 || width > MaxSide || height > MaxSide {
		return nil, fmt.Errorf("%w: %dx%d is outside 1 to %d", ErrPixels, width, height, MaxSide)
	}
	if len(rgb) != width*height*3 {
		return nil, fmt.Errorf("%w: %d bytes for %dx%d RGB24", ErrPixels, len(rgb), width, height)
	}
	// An opaque RGBA image is written as truecolour without alpha.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i, j := 0, 0; i < len(rgb); i, j = i+3, j+4 {
		img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = rgb[i], rgb[i+1], rgb[i+2], 0xFF
	}
	var out bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.DefaultCompression}).Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
