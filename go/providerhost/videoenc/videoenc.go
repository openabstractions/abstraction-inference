// Package videoenc encodes raw video frames as H.264 in an MP4 with the
// platform's own encoder, so a provider whose engine returns frames delivers
// a video that browsers and common players open (inference CONTRACT.md
// INF-VE1). On Windows it drives Media Foundation's Sink Writer, which ships
// with Windows, through OA's own COM binding: the hardware H.264 encoder when
// the machine has one, Microsoft's software encoder otherwise, and the
// Encoded says which ran. Every other platform answers ErrNoSystemEncoder; no
// fallback encoder is bundled.
package videoenc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MP4MediaType is the delivery's media type.
const MP4MediaType = "video/mp4"

// ErrNoSystemEncoder is the answer of a platform with no system H.264 encoder
// this package binds: everywhere but Windows.
var ErrNoSystemEncoder = errors.New("videoenc: H.264 MP4 encoding needs Windows Media Foundation, which this platform does not have")

// ErrOptions wraps every refusal of an Options or a frame.
var ErrOptions = errors.New("videoenc: options refused")

// A PixelFormat is the layout of one frame's bytes: rows top to bottom,
// pixels left to right, no padding between rows.
type PixelFormat int

const (
	// RGB24 is three bytes per pixel, red, green, blue.
	RGB24 PixelFormat = iota + 1
	// RGBA is four bytes per pixel, red, green, blue, alpha; alpha is
	// dropped.
	RGBA
)

// bytesPerPixel is the size of one pixel of f, or 0 for no format.
func (f PixelFormat) bytesPerPixel() int {
	switch f {
	case RGB24:
		return 3
	case RGBA:
		return 4
	}
	return 0
}

// Bounds of a stream: H.264 codes 4:2:0 chroma, so both sides are even;
// Microsoft's software encoder takes at most 4096x2304.
const (
	minSide = 32
	maxSide = 4096
	maxFPS  = 240
)

// Options describe one video.
type Options struct {
	Width, Height int
	// FPS is the frame rate; each frame lasts 1/FPS seconds.
	FPS    int
	Layout PixelFormat
	// Bitrate is the average bit rate in bits per second; zero reads
	// DefaultBitrate.
	Bitrate int
	// Software asks for Microsoft's software encoder even where a hardware
	// encoder exists.
	Software bool
}

// DefaultBitrate is the average rate a video of the given size and rate
// is encoded at: 0.4 bits per pixel per frame and at least 1 Mbit/s, which
// keeps a 512x512 clip at 16 fps at 1.7 Mbit/s.
func DefaultBitrate(width, height, fps int) int {
	return max(1_000_000, width*height*fps*2/5)
}

// check refuses options the encoder cannot honour.
func (o Options) check() error {
	switch {
	case o.Width < minSide || o.Height < minSide || o.Width > maxSide || o.Height > maxSide:
		return fmt.Errorf("%w: %dx%d is outside %d to %d", ErrOptions, o.Width, o.Height, minSide, maxSide)
	case o.Width%2 != 0 || o.Height%2 != 0:
		return fmt.Errorf("%w: %dx%d has an odd side; H.264 4:2:0 needs even sides", ErrOptions, o.Width, o.Height)
	case o.FPS < 1 || o.FPS > maxFPS:
		return fmt.Errorf("%w: %d fps is outside 1 to %d", ErrOptions, o.FPS, maxFPS)
	case o.Layout.bytesPerPixel() == 0:
		return fmt.Errorf("%w: no pixel format", ErrOptions)
	case o.Bitrate < 0:
		return fmt.Errorf("%w: a negative bit rate", ErrOptions)
	case uint64(o.Bitrate) > math.MaxUint32:
		// MF_MT_AVG_BITRATE is a UINT32.
		return fmt.Errorf("%w: a bit rate of %d passes %d", ErrOptions, o.Bitrate, uint64(math.MaxUint32))
	}
	return nil
}

// frameBytes is the size of one input frame.
func (o Options) frameBytes() int { return o.Width * o.Height * o.Layout.bytesPerPixel() }

// frameTime is the start of frame i in 100-nanosecond units, the unit Media
// Foundation and the MP4 sink count in.
func frameTime(i, fps int) int64 { return int64(i) * 10_000_000 / int64(fps) }

// bgrx writes one frame into dst as 32-bit BGRX rows, top to bottom: Media
// Foundation's RGB32 with a positive stride.
func bgrx(dst, src []byte, o Options) {
	step := o.Layout.bytesPerPixel()
	for i, j := 0, 0; i+step <= len(src) && j+4 <= len(dst); i, j = i+step, j+4 {
		dst[j], dst[j+1], dst[j+2], dst[j+3] = src[i+2], src[i+1], src[i], 0xFF
	}
}

// Encoded says what was written and which encoder wrote it.
type Encoded struct {
	Path     string
	Frames   int
	Duration time.Duration
	// Hardware is true when the H.264 encoder that ran is a hardware
	// encoder (Media Foundation names a hardware URL for it).
	Hardware bool
	// EncoderName is the encoder's friendly name when Media Foundation
	// gives one, else "hardware H.264 encoder" or "software H.264 encoder".
	EncoderName string
	// FastStart is true when moov comes before mdat. It is false when
	// moving moov failed after the encoder finished: the video is then
	// placed as the encoder wrote it, moov last, and still plays once
	// downloaded.
	FastStart bool
}

// checkPath refuses a destination that is not an absolute .mp4 path in an
// existing folder, and one where anything already stands: the video is
// moved into place only where nothing is (INF-VE1).
func checkPath(path string) error {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".mp4") {
		return fmt.Errorf("%w: %s is not an absolute .mp4 path", ErrOptions, path)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: the folder of %s does not exist", ErrOptions, path)
	}
	if _, err := os.Lstat(path); err == nil {
		return &fs.PathError{Op: "create", Path: path, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// tempName is a fresh name beside path that the writer writes before Close
// moves it into place: a dot, path's base, a random part and .mp4.
func tempName(path string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return filepath.Join(filepath.Dir(path), "."+base+"-"+hex.EncodeToString(raw[:])+".part.mp4"), nil
}

// EncodeFile writes frames to path as one MP4 and returns what it wrote. A
// failure leaves no file of its own.
func EncodeFile(path string, o Options, frames [][]byte) (Encoded, error) {
	e, err := Create(path, o)
	if err != nil {
		return Encoded{}, err
	}
	for _, f := range frames {
		if err := e.Write(f); err != nil {
			e.Abort()
			return Encoded{}, err
		}
	}
	return e.Close()
}

// Encode returns frames as the bytes of one MP4. The Sink Writer writes a
// seekable file, so the MP4 passes through a private temporary file in dir
// (the system's temporary folder when dir is empty), removed before Encode
// returns.
func Encode(dir string, o Options, frames [][]byte) ([]byte, Encoded, error) {
	if err := o.check(); err != nil {
		return nil, Encoded{}, err
	}
	temp, err := os.MkdirTemp(dir, "oa-videoenc-")
	if err != nil {
		return nil, Encoded{}, err
	}
	//unchecked: a private temporary folder; a failed removal leaves only that folder
	defer os.RemoveAll(temp)
	path := filepath.Join(temp, "video.mp4")
	result, err := EncodeFile(path, o, frames)
	if err != nil {
		return nil, Encoded{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Encoded{}, err
	}
	result.Path = ""
	return data, result, nil
}
