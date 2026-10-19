package rgbpng

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"net/http"
	"testing"
)

// INF-IO1: the PNG decodes back to exactly the pixels given, as an opaque
// 8-bit truecolour image whose sniffed type is image/png.
func TestEncodeRoundTripsThePixels(t *testing.T) {
	const w, h = 37, 19
	rgb := make([]byte, w*h*3)
	for i := range rgb {
		rgb[i] = byte(i*7 + i/3)
	}
	data, err := Encode(w, h, rgb)
	if err != nil {
		t.Fatal(err)
	}
	if got := http.DetectContentType(data); got != PNGMediaType {
		t.Fatalf("sniffed %q", got)
	}
	// IHDR: width, height, bit depth 8, colour type 2 (truecolour).
	if binary.BigEndian.Uint32(data[16:]) != w || binary.BigEndian.Uint32(data[20:]) != h || data[24] != 8 || data[25] != 2 {
		t.Fatalf("IHDR % x", data[16:26])
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, w, h) {
		t.Fatalf("bounds %v", img.Bounds())
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			i := (y*w + x) * 3
			if byte(r>>8) != rgb[i] || byte(g>>8) != rgb[i+1] || byte(b>>8) != rgb[i+2] || a != 0xFFFF {
				t.Fatalf("pixel %d,%d", x, y)
			}
		}
	}
}

// INF-IO1: a size outside the bounds or a buffer of the wrong length is
// refused before anything is written.
func TestEncodeRefusesWrongPixels(t *testing.T) {
	for name, c := range map[string]struct {
		w, h, n int
	}{
		"short":  {4, 4, 47},
		"long":   {4, 4, 49},
		"zero":   {0, 4, 0},
		"huge":   {MaxSide + 1, 1, (MaxSide + 1) * 3},
		"negate": {-4, -4, 48},
	} {
		if _, err := Encode(c.w, c.h, make([]byte, c.n)); !errors.Is(err, ErrPixels) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
