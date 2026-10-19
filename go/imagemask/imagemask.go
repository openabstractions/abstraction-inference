// Package imagemask converts an image edit mask from the image contract's
// form (inference CONTRACT.md INF-I1: a PNG whose fully transparent pixels
// mark the area to change) to the form an engine reads. It is the one place
// inference's adapters and the providers convert a mask.
package imagemask

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// ErrMask reports a mask that is not a decodable PNG or a form this package
// does not write.
var ErrMask = errors.New("imagemask: the mask is no decodable PNG")

// Form is how an engine reads an edit mask.
type Form int

const (
	// Alpha is the contract's own reading, which OpenAI-style edit routes
	// share: alpha 0 marks the area to change. The converted mask also
	// paints that area white and every other pixel opaque black, so a
	// server behind an OpenAI-compatible route that reads luminance
	// instead (stable-diffusion.cpp's sd-server, Lemonade) reads the same
	// area.
	Alpha Form = iota + 1
	// Luminance is a grayscale mask whose white pixels mark the area to
	// change and black pixels the area kept: sd-server's edit route and
	// Stability's inpaint route read the mask's brightness.
	Luminance
)

// Convert decodes a contract mask and returns it in form, with its width and
// height. The caller bounds the mask's header first (sdcpp.CheckImageInput),
// so decoding allocates at most that bound's pixels.
func Convert(form Form, data []byte) ([]byte, int, int, error) {
	if form != Alpha && form != Luminance {
		return nil, 0, 0, fmt.Errorf("%w: form %d", ErrMask, form)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %v", ErrMask, err)
	}
	bounds := decoded.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	var out image.Image
	switch form {
	case Luminance:
		gray := image.NewGray(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				if changes(decoded, bounds.Min.X+x, bounds.Min.Y+y) {
					gray.Pix[y*gray.Stride+x] = 255
				}
			}
		}
		out = gray
	case Alpha:
		rgba := image.NewNRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				c := color.NRGBA{A: 255}
				if changes(decoded, bounds.Min.X+x, bounds.Min.Y+y) {
					c = color.NRGBA{R: 255, G: 255, B: 255}
				}
				rgba.SetNRGBA(x, y, c)
			}
		}
		out = rgba
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, 0, 0, fmt.Errorf("%w: %v", ErrMask, err)
	}
	return buf.Bytes(), width, height, nil
}

// Inspect decodes a contract mask and returns its width, height and the
// number of pixels it marks to change. The caller bounds the header first.
func Inspect(data []byte) (int, int, int, error) {
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %v", ErrMask, err)
	}
	bounds := decoded.Bounds()
	changed := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if changes(decoded, x, y) {
				changed++
			}
		}
	}
	return bounds.Dx(), bounds.Dy(), changed, nil
}

// changes reports a fully transparent pixel, the contract's area to change.
func changes(m image.Image, x, y int) bool {
	_, _, _, alpha := m.At(x, y).RGBA()
	return alpha == 0
}
