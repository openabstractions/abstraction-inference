package videoenc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// box is one ISO base media file format box (ISO/IEC 14496-12 §4.2).
type box struct {
	kind string
	body []byte
}

// boxes splits data into its boxes: a 32-bit size and four-character type,
// a 64-bit size after the type when the size is 1, and the rest of data
// when the size is 0.
func boxes(data []byte) ([]box, error) {
	var out []box
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, fmt.Errorf("%d bytes after the last box", len(data))
		}
		size, header := uint64(binary.BigEndian.Uint32(data)), uint64(8)
		kind := string(data[4:8])
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return nil, fmt.Errorf("box %q has no 64-bit size", kind)
			}
			size, header = binary.BigEndian.Uint64(data[8:]), 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, fmt.Errorf("box %q of %d bytes in %d", kind, size, len(data))
		}
		out = append(out, box{kind: kind, body: data[header:size]})
		data = data[size:]
	}
	return out, nil
}

func child(parent []box, kind string) []byte {
	for _, b := range parent {
		if b.kind == kind {
			return b.body
		}
	}
	return nil
}

// mp4 is what the tests read from a file: its top-level box types, the
// movie's duration, and the video track's sample count and duration from
// its media header, its sample sizes and its time-to-sample table.
type mp4 struct {
	top                    []string
	brand                  string
	movieSeconds           float64
	samples                int
	trackSeconds           float64
	sttsSamples            int
	sttsSeconds            float64
	width, height          int
	videoTracks, allTracks int
}

// timing reads a full box's version-dependent time scale and duration
// (mvhd and mdhd, §8.2.2 and §8.4.2).
func timing(body []byte) (scale uint32, duration uint64, err error) {
	if len(body) < 4 {
		return 0, 0, errors.New("short header")
	}
	if body[0] == 1 {
		if len(body) < 32 {
			return 0, 0, errors.New("short version 1 header")
		}
		return binary.BigEndian.Uint32(body[20:]), binary.BigEndian.Uint64(body[24:]), nil
	}
	if len(body) < 20 {
		return 0, 0, errors.New("short version 0 header")
	}
	return binary.BigEndian.Uint32(body[12:]), uint64(binary.BigEndian.Uint32(body[16:])), nil
}

func parseMP4(data []byte) (mp4, error) {
	var m mp4
	top, err := boxes(data)
	if err != nil {
		return m, err
	}
	for _, b := range top {
		m.top = append(m.top, b.kind)
	}
	if ftyp := child(top, "ftyp"); len(ftyp) >= 4 {
		m.brand = string(ftyp[:4])
	}
	moov, err := boxes(child(top, "moov"))
	if err != nil {
		return m, fmt.Errorf("moov: %w", err)
	}
	scale, duration, err := timing(child(moov, "mvhd"))
	if err != nil || scale == 0 {
		return m, fmt.Errorf("mvhd: %v", err)
	}
	m.movieSeconds = float64(duration) / float64(scale)
	for _, trak := range moov {
		if trak.kind != "trak" {
			continue
		}
		m.allTracks++
		parts, err := boxes(trak.body)
		if err != nil {
			return m, err
		}
		mdia, err := boxes(child(parts, "mdia"))
		if err != nil {
			return m, err
		}
		if hdlr := child(mdia, "hdlr"); len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
			continue
		}
		m.videoTracks++
		if tkhd := child(parts, "tkhd"); len(tkhd) >= 84 {
			// The last eight bytes are width and height in 16.16 fixed point.
			m.width = int(binary.BigEndian.Uint32(tkhd[len(tkhd)-8:]) >> 16)
			m.height = int(binary.BigEndian.Uint32(tkhd[len(tkhd)-4:]) >> 16)
		}
		scale, duration, err := timing(child(mdia, "mdhd"))
		if err != nil || scale == 0 {
			return m, fmt.Errorf("mdhd: %v", err)
		}
		m.trackSeconds = float64(duration) / float64(scale)
		minf, err := boxes(child(mdia, "minf"))
		if err != nil {
			return m, err
		}
		stbl, err := boxes(child(minf, "stbl"))
		if err != nil {
			return m, err
		}
		if stsz := child(stbl, "stsz"); len(stsz) >= 12 {
			m.samples = int(binary.BigEndian.Uint32(stsz[8:]))
		}
		stts := child(stbl, "stts")
		if len(stts) < 8 {
			return m, errors.New("no stts")
		}
		entries := int(binary.BigEndian.Uint32(stts[4:]))
		if len(stts) < 8+8*entries {
			return m, errors.New("short stts")
		}
		var ticks uint64
		for i := 0; i < entries; i++ {
			count := binary.BigEndian.Uint32(stts[8+8*i:])
			delta := binary.BigEndian.Uint32(stts[12+8*i:])
			m.sttsSamples += int(count)
			ticks += uint64(count) * uint64(delta)
		}
		m.sttsSeconds = float64(ticks) / float64(scale)
	}
	return m, nil
}

// frame is synthetic frame k of w x h in format: a red ramp across, a
// green ramp down, blue against both, and a white square moving 8 pixels
// a frame, so a swapped channel, a flipped image or a wrong frame shows.
func frame(k, w, h int, format PixelFormat) []byte {
	step := format.bytesPerPixel()
	out := make([]byte, w*h*step)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * step
			r, g, b := byte(x*255/(w-1)), byte(y*255/(h-1)), byte(255-(x+y)*255/(w+h-2))
			if x >= 16+8*k && x < 48+8*k && y >= 16 && y < 48 {
				r, g, b = 255, 255, 255
			}
			out[i], out[i+1], out[i+2] = r, g, b
			if step == 4 {
				out[i+3] = 255
			}
		}
	}
	return out
}

// psnr compares two RGB24 frames over every channel, in decibels.
func psnr(a, b []byte) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	if sum == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/(sum/float64(len(a))))
}

// INF-VE1: options outside the encoder's bounds are refused by name before
// anything starts, on every platform.
func TestOptionsAreRefusedBeforeAnythingStarts(t *testing.T) {
	ok := Options{Width: 256, Height: 144, FPS: 16, Layout: RGB24}
	path := filepath.Join(t.TempDir(), "v.mp4")
	for name, o := range map[string]Options{
		"odd width":    {Width: 255, Height: 144, FPS: 16, Layout: RGB24},
		"tiny":         {Width: 16, Height: 16, FPS: 16, Layout: RGB24},
		"huge":         {Width: 8192, Height: 144, FPS: 16, Layout: RGB24},
		"no rate":      {Width: 256, Height: 144, Layout: RGB24},
		"no format":    {Width: 256, Height: 144, FPS: 16},
		"bad bitrate":  {Width: 256, Height: 144, FPS: 16, Layout: RGB24, Bitrate: -1},
		"huge bitrate": {Width: 256, Height: 144, FPS: 16, Layout: RGB24, Bitrate: int(min(uint64(math.MaxUint32)+1, uint64(math.MaxInt)))},
	} {
		if _, err := Create(path, o); !errors.Is(err, ErrOptions) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, p := range []string{"v.mp4", filepath.Join(t.TempDir(), "v.avi"), filepath.Join(t.TempDir(), "missing", "v.mp4")} {
		if _, err := Create(p, ok); !errors.Is(err, ErrOptions) {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if DefaultBitrate(512, 512, 16) != 1677721 || DefaultBitrate(64, 64, 16) != 1_000_000 {
		t.Fatalf("default bit rates %d %d", DefaultBitrate(512, 512, 16), DefaultBitrate(64, 64, 16))
	}
}

// mkbox is one box of kind holding body.
func mkbox(kind string, body ...[]byte) []byte {
	var all []byte
	for _, b := range body {
		all = append(all, b...)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(all)))
	return append(append(out, kind...), all...)
}

// INF-VE1, fast start: moov moves before mdat and each chunk offset grows
// by moov's size, so it still names the same media bytes.
func TestFastStartMovesMoovAndItsOffsets(t *testing.T) {
	ftyp := mkbox("ftyp", []byte("isom\x00\x00\x02\x00"))
	mdat := mkbox("mdat", []byte("AAAABBBB"))
	first := uint32(len(ftyp) + 8) // "AAAA" after mdat's header
	stco := mkbox("stco", []byte{0, 0, 0, 0}, binary.BigEndian.AppendUint32(nil, 2),
		binary.BigEndian.AppendUint32(nil, first), binary.BigEndian.AppendUint32(nil, first+4))
	moov := mkbox("moov", mkbox("mvhd", make([]byte, 20)), mkbox("trak", mkbox("mdia", mkbox("minf", mkbox("stbl", stco)))))
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.mp4"), filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(in, append(append(ftyp, mdat...), moov...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fastStart(in, out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	top, err := boxes(data)
	if err != nil || len(top) != 3 || top[0].kind != "ftyp" || top[1].kind != "moov" || top[2].kind != "mdat" {
		t.Fatalf("boxes %v %v", top, err)
	}
	// The offsets inside the moved moov: stco is its last box.
	tail := data[len(ftyp)+len(moov)-8 : len(ftyp)+len(moov)]
	for i, want := range []string{"AAAA", "BBBB"} {
		offset := binary.BigEndian.Uint32(tail[4*i:])
		if got := string(data[offset : offset+4]); got != want {
			t.Fatalf("chunk %d at %d reads %q, want %q", i, offset, got, want)
		}
	}
	if err := fastStart(in, out); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("an existing output: %v", err)
	}
}

// Frames are timed in 100 ns units without drift: frame i starts at
// i/fps seconds exactly, rounded down.
func TestFrameTimes(t *testing.T) {
	if frameTime(16, 16) != 10_000_000 || frameTime(1, 30) != 333_333 || frameTime(30, 30) != 10_000_000 {
		t.Fatal("frame times drift")
	}
	src := []byte{1, 2, 3, 4, 5, 6}
	dst := make([]byte, 8)
	bgrx(dst, src, Options{Layout: RGB24})
	if string(dst) != string([]byte{3, 2, 1, 255, 6, 5, 4, 255}) {
		t.Fatalf("BGRX %v", dst)
	}
}
