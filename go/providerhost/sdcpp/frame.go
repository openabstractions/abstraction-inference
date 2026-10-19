package sdcpp

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// ErrFirstFrame reports a caller's first frame the provider will not
// decode (video addendum condition 8; inference CONTRACT.md INF-SD8).
var ErrFirstFrame = errors.New("sd.cpp: first frame refused")

// Bounds of a caller's first frame before the provider decodes it.
const (
	MaxFirstFrameBytes = 16 << 20
	maxFirstFrameSide  = 4096
)

// FirstFrame decodes a caller's PNG or JPEG first frame, at most
// MaxFirstFrameBytes and maxFirstFrameSide on each side by its header,
// crops its centre to the aspect of width × height, scales it to exactly
// that size, flattens any transparency onto black and returns the
// provider's own 8-bit PNG. sd-server then decodes only an OA-made PNG of the
// expected size, which keeps finding V1's resize path and stb_image's other
// decoders out of the caller's reach. width and height must be a reviewed
// video size.
func FirstFrame(media string, data []byte, width, height int) ([]byte, error) {
	if err := CheckVideoSize(width, height); err != nil {
		return nil, err
	}
	if media != "image/png" && media != "image/jpeg" {
		return nil, fmt.Errorf("%w: %q is not PNG or JPEG", ErrFirstFrame, media)
	}
	if len(data) == 0 || len(data) > MaxFirstFrameBytes {
		return nil, fmt.Errorf("%w: the frame is empty or exceeds %d bytes", ErrFirstFrame, MaxFirstFrameBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != media {
		return nil, fmt.Errorf("%w: the bytes are not %s", ErrFirstFrame, media)
	}
	if config.Width < 1 || config.Height < 1 || config.Width > maxFirstFrameSide || config.Height > maxFirstFrameSide {
		return nil, fmt.Errorf("%w: %dx%d is beyond %d on a side", ErrFirstFrame, config.Width, config.Height, maxFirstFrameSide)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: the %s does not decode", ErrFirstFrame, media)
	}
	crop := centreCrop(src.Bounds(), width, height)
	flat := image.NewNRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), src, crop.Min, draw.Over)
	out := resample(flat, width, height)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFirstFrame, err)
	}
	return encoded.Bytes(), nil
}

// centreCrop is the largest rectangle of b's centre with width:height's
// aspect.
func centreCrop(b image.Rectangle, width, height int) image.Rectangle {
	w, h := b.Dx(), b.Dy()
	if w*height > h*width {
		cw := max(1, (h*width+height/2)/height)
		x := b.Min.X + (w-cw)/2
		return image.Rect(x, b.Min.Y, x+cw, b.Max.Y)
	}
	ch := max(1, (w*height+width/2)/width)
	y := b.Min.Y + (h-ch)/2
	return image.Rect(b.Min.X, y, b.Max.X, y+ch)
}

// resample scales src to width × height with a separable triangle filter:
// bilinear when enlarging, and averaging over the covered source pixels
// when reducing. Every output pixel is opaque.
func resample(src *image.NRGBA, width, height int) *image.NRGBA {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	// Horizontal pass: sh rows of width pixels, three channels.
	horizontal := make([]float32, sh*width*3)
	weights := filterWeights(sw, width)
	for y := 0; y < sh; y++ {
		row := src.Pix[y*src.Stride:]
		for x, w := range weights {
			var r, g, b float32
			for i, k := range w.k {
				p := (w.first + i) * 4
				r += k * float32(row[p])
				g += k * float32(row[p+1])
				b += k * float32(row[p+2])
			}
			o := (y*width + x) * 3
			horizontal[o], horizontal[o+1], horizontal[o+2] = r, g, b
		}
	}
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y, w := range filterWeights(sh, height) {
		for x := 0; x < width; x++ {
			var r, g, b float32
			for i, k := range w.k {
				p := ((w.first+i)*width + x) * 3
				r += k * horizontal[p]
				g += k * horizontal[p+1]
				b += k * horizontal[p+2]
			}
			o := y*out.Stride + x*4
			out.Pix[o], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = clamp8(r), clamp8(g), clamp8(b), 255
		}
	}
	return out
}

// weight is one output sample's source span and normalised weights.
type weight struct {
	first int
	k     []float32
}

// filterWeights are the triangle-filter weights mapping n source samples to
// m output samples.
func filterWeights(n, m int) []weight {
	scale := float64(n) / float64(m)
	support := max(scale, 1)
	out := make([]weight, m)
	for i := range out {
		centre := (float64(i)+0.5)*scale - 0.5
		first := max(0, int(centre-support)+1)
		last := min(n-1, int(centre+support))
		if last < first {
			first, last = min(max(0, int(centre+0.5)), n-1), min(max(0, int(centre+0.5)), n-1)
		}
		k := make([]float32, last-first+1)
		var sum float64
		for j := range k {
			d := float64(first+j) - centre
			if d < 0 {
				d = -d
			}
			v := 1 - d/support
			if v < 0 {
				v = 0
			}
			k[j] = float32(v)
			sum += v
		}
		if sum == 0 {
			k = []float32{1}
			first = min(max(0, int(centre+0.5)), n-1)
			sum = 1
		}
		for j := range k {
			k[j] = float32(float64(k[j]) / sum)
		}
		out[i] = weight{first: first, k: k}
	}
	return out
}

func clamp8(v float32) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	}
	return uint8(v + 0.5)
}
