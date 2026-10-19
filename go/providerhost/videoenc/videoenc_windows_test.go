//go:build windows

package videoenc

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// INF-VE1: Abort while four writers are in flight never panics and leaves
// no file, round after round.
func TestAbortWhileWritersRun(t *testing.T) {
	o := Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true}
	frame := frame(0, testW, testH, RGB24)
	for round := range 20 {
		dir := t.TempDir()
		e, err := Create(filepath.Join(dir, "v.mp4"), o)
		if errors.Is(err, ErrNoSystemEncoder) {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for e.Write(frame) == nil {
				}
			}()
		}
		time.Sleep(time.Duration(round%5) * 5 * time.Millisecond)
		e.Abort()
		wg.Wait()
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("round %d left %v", round, entries)
		}
	}
}

// INF-VE1: concurrent Writes each take their own frame index on the
// encoder's thread: 32 writers make 32 samples over 2 s at 16 fps.
func TestConcurrentWritesAreOrderedByArrival(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.mp4")
	e, err := Create(path, Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true})
	if errors.Is(err, ErrNoSystemEncoder) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for k := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.Write(frame(k%12, testW, testH, RGB24))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := e.Close()
	if err != nil || r.Frames != 32 || r.Duration != 2*time.Second {
		t.Fatalf("result %+v %v", r, err)
	}
	checkMP4(t, path, 32, true)
}

// INF-VE1: the MP4 is moved into place only where nothing stands: a path
// taken before Create is refused, and one taken before Close keeps its file
// while the video's temporary file is removed.
func TestTheVideoNeverReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taken.mp4")
	if err := os.WriteFile(path, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true}
	if _, err := Create(path, o); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("an existing path: %v", err)
	}
	later := filepath.Join(dir, "later.mp4")
	e, err := Create(later, o)
	if errors.Is(err, ErrNoSystemEncoder) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Write(frame(0, testW, testH, RGB24)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(later, []byte("arrived meanwhile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Close(); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a path taken before Close: %v", err)
	}
	if data, _ := os.ReadFile(later); string(data) != "arrived meanwhile" {
		t.Fatalf("the file that arrived was replaced: %q", data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("the temporary file stayed: %v", entries)
	}
}

// The test video: 256x144 (both sides multiples of 16, so no decoder
// padding) at 16 fps.
const testW, testH, testFPS = 256, 144, 16

// minPSNR is the decoded frame's floor against its source. The RGB to
// 4:2:0 YUV round trip alone, on these smooth ramps, stays near 40 dB; at
// DefaultBitrate's 1 Mbit/s for 256x144 (1.7 bits per pixel) H.264 adds
// little to it. A swapped channel, an upside-down image or a frame from
// another time reads under 20 dB, so 30 dB separates a working path from a
// broken one with room for encoder differences.
const minPSNR = 30.0

// encodeTimeLimit bounds one encode of a few frames, so a hung encoder
// fails the test rather than the run.
const encodeTimeLimit = 60 * time.Second

func encodeWithin(t *testing.T, path string, o Options, frames [][]byte) Encoded {
	t.Helper()
	type outcome struct {
		r   Encoded
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := EncodeFile(path, o, frames)
		done <- outcome{r, err}
	}()
	select {
	case got := <-done:
		if errors.Is(got.err, ErrNoSystemEncoder) {
			t.Skipf("no Media Foundation on this Windows: %v", got.err)
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.r
	case <-time.After(encodeTimeLimit):
		t.Fatalf("the encode passed %s", encodeTimeLimit)
	}
	return Encoded{}
}

func testFrames(n int, format PixelFormat) [][]byte {
	out := make([][]byte, n)
	for k := range out {
		out[k] = frame(k, testW, testH, format)
	}
	return out
}

// decodeFrames reads every frame of the MP4 at path back through Media
// Foundation's Source Reader, converted to RGB32 by its video processing,
// and returns them as RGB24.
func decodeFrames(t *testing.T, path string) [][]byte {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	stop, err := startup()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	attributes, err := newAttributes(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attributes.release()
	if err := attributes.setUINT32(&mfReaderVideoProcess, 1); err != nil {
		t.Fatal(err)
	}
	url, err := utf16(path)
	if err != nil {
		t.Fatal(err)
	}
	var reader *comObject
	if err := proc(procMFCreateSourceReaderFromURL, "MFCreateSourceReaderFromURL", uintptr(unsafe.Pointer(url)),
		uintptr(unsafe.Pointer(attributes)), uintptr(unsafe.Pointer(&reader))); err != nil {
		t.Fatal(err)
	}
	defer reader.release()
	want, err := newMediaType()
	if err != nil {
		t.Fatal(err)
	}
	defer want.release()
	if err := errors.Join(want.setGUID(&mfMTMajorType, &mfMediaTypeVideo), want.setGUID(&mfMTSubtype, &mfVideoFormatRGB32)); err != nil {
		t.Fatal(err)
	}
	if err := check("IMFSourceReader::SetCurrentMediaType", reader.call(slotSetCurrentMediaType, firstVideoStream, 0, uintptr(unsafe.Pointer(want)))); err != nil {
		t.Fatal(err)
	}
	// The decoded type's stride: positive is top-down rows, negative
	// bottom-up; RGB with no stride attribute is bottom-up (Media
	// Foundation's "Image Stride").
	stride := -int32(testW * 4)
	var current *comObject
	if err := check("IMFSourceReader::GetCurrentMediaType", reader.call(slotGetCurrentMediaType, firstVideoStream, uintptr(unsafe.Pointer(&current)))); err != nil {
		t.Fatal(err)
	}
	var value uint32
	if !failed(current.call(slotGetUINT32, uintptr(unsafe.Pointer(&mfMTDefaultStride)), uintptr(unsafe.Pointer(&value)))) {
		stride = int32(value)
	}
	current.release()
	t.Logf("decoded stride %d", stride)
	var out [][]byte
	for len(out) < 1000 {
		var stream, flags uint32
		var timestamp int64
		var sample *comObject
		if err := check("IMFSourceReader::ReadSample", reader.call(slotReadSample, firstVideoStream, 0, uintptr(unsafe.Pointer(&stream)),
			uintptr(unsafe.Pointer(&flags)), uintptr(unsafe.Pointer(&timestamp)), uintptr(unsafe.Pointer(&sample)))); err != nil {
			t.Fatal(err)
		}
		if sample != nil {
			out = append(out, samplePixels(t, sample, stride))
			sample.release()
		}
		if flags&readerEndOfStream != 0 {
			break
		}
	}
	return out
}

// samplePixels copies one decoded RGB32 sample as RGB24, row by row: with
// the 2D buffer's own pitch when the buffer is one, and otherwise from a
// locked buffer with the media type's stride, either of which may be
// negative (bottom-up).
func samplePixels(t *testing.T, sample *comObject, stride int32) []byte {
	t.Helper()
	var buffer *comObject
	if err := check("IMFSample::ConvertToContiguousBuffer", sample.call(slotConvertToContiguousBuffer, uintptr(unsafe.Pointer(&buffer)))); err != nil {
		t.Fatal(err)
	}
	defer buffer.release()
	var scan0 *byte
	var pitch int32
	var buffer2D *comObject
	if !failed(buffer.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidIMF2DBuffer)), uintptr(unsafe.Pointer(&buffer2D)))) && buffer2D != nil {
		defer buffer2D.release()
		if err := check("IMF2DBuffer::Lock2D", buffer2D.call(slotLock2D, uintptr(unsafe.Pointer(&scan0)), uintptr(unsafe.Pointer(&pitch)))); err != nil {
			t.Fatal(err)
		}
		defer buffer2D.call(slotUnlock2D)
	} else {
		var data *byte
		var length uint32
		if err := check("IMFMediaBuffer::Lock", buffer.call(slotLock, uintptr(unsafe.Pointer(&data)), 0, uintptr(unsafe.Pointer(&length)))); err != nil {
			t.Fatal(err)
		}
		defer buffer.call(slotUnlock)
		if int(length) < testW*testH*4 {
			t.Fatalf("a decoded frame of %d bytes", length)
		}
		scan0, pitch = data, stride
		if stride < 0 {
			// Bottom-up: the first row of the picture is the buffer's last.
			scan0 = (*byte)(unsafe.Add(unsafe.Pointer(data), (testH-1)*testW*4))
		}
	}
	out := make([]byte, 0, testW*testH*3)
	for y := 0; y < testH; y++ {
		row := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(scan0), y*int(pitch))), testW*4)
		for x := 0; x < testW; x++ {
			out = append(out, row[4*x+2], row[4*x+1], row[4*x])
		}
	}
	return out
}

// checkMP4 reads the MP4 at path: one H.264 track of frames frames at the
// test size and rate, moov before mdat when fast and after it otherwise.
func checkMP4(t *testing.T, path string, frames int, fast bool) mp4 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseMP4(data)
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, kind := range m.top {
		has[kind] = true
	}
	if len(m.top) == 0 || m.top[0] != "ftyp" || !has["moov"] || !has["mdat"] {
		t.Fatalf("top-level boxes %v", m.top)
	}
	// Fast start: moov before mdat, so a browser plays while downloading.
	if leads := slices.Index(m.top, "moov") < slices.Index(m.top, "mdat"); leads != fast {
		t.Fatalf("moov before mdat %v, want %v: %v", leads, fast, m.top)
	}
	seconds := float64(frames) / testFPS
	frame := 1.0 / testFPS
	if m.videoTracks != 1 || m.allTracks != 1 || m.samples != frames || m.sttsSamples != frames ||
		math.Abs(m.trackSeconds-seconds) > frame/2 || math.Abs(m.sttsSeconds-seconds) > frame/2 || math.Abs(m.movieSeconds-seconds) > frame/2 ||
		m.width != testW || m.height != testH {
		t.Fatalf("mp4 %+v, want %d frames over %.4f s at %dx%d", m, frames, seconds, testW, testH)
	}
	return m
}

// INF-VE1: Microsoft's software encoder writes an MP4 of one H.264 video
// track whose box structure, frame count and duration are the frames
// given, and whose frames decode back through Media Foundation's Source
// Reader within minPSNR of their sources.
func TestSoftwareEncoderWritesAPlayableMP4(t *testing.T) {
	const n = 12
	path := filepath.Join(t.TempDir(), "software.mp4")
	sources := testFrames(n, RGB24)
	r := encodeWithin(t, path, Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true}, sources)
	if r.Hardware || r.Frames != n || r.Duration != 750*time.Millisecond || r.Path != path || r.EncoderName == "" || !r.FastStart {
		t.Fatalf("result %+v", r)
	}
	t.Logf("encoder: %s", r.EncoderName)
	m := checkMP4(t, path, n, true)
	t.Logf("mp4: brand %s, boxes %v, %.4f s", m.brand, m.top, m.trackSeconds)
	decoded := decodeFrames(t, path)
	if len(decoded) != n {
		t.Fatalf("decoded %d frames of %d", len(decoded), n)
	}
	for _, k := range []int{0, 5, n - 1} {
		got := psnr(decoded[k], sources[k])
		t.Logf("frame %d: PSNR %.2f dB", k, got)
		if got < minPSNR {
			t.Fatalf("frame %d: PSNR %.2f dB under %.0f", k, got, minPSNR)
		}
	}
	// The square moves: a frame compared with the next one's source is far
	// below the floor, so the floor tells frames apart.
	if wrong := psnr(decoded[5], sources[9]); wrong >= minPSNR {
		t.Fatalf("frame 5 against frame 9's source: %.2f dB", wrong)
	}
}

// INF-VE1, review of 0410d167f: a fast start that fails after the encoder
// finished never discards the video. The encoder's own file, moov last, is
// placed at the path, the result reads FastStart false, its frames decode,
// and no temporary file is left beside it.
func TestFailedFastStartPlacesTheEncodersFile(t *testing.T) {
	saved := fastStartFile
	t.Cleanup(func() { fastStartFile = saved })
	fastStartFile = func(in, out string) error {
		// A partial copy is left behind, as a failure midway would.
		if err := os.WriteFile(out, []byte("partial"), 0o600); err != nil {
			return err
		}
		return errors.New("forced fast start failure")
	}
	const n = 6
	dir := t.TempDir()
	path := filepath.Join(dir, "moov-last.mp4")
	r := encodeWithin(t, path, Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true}, testFrames(n, RGB24))
	if r.FastStart || r.Frames != n || r.Path != path {
		t.Fatalf("result %+v", r)
	}
	checkMP4(t, path, n, false)
	if decoded := decodeFrames(t, path); len(decoded) != n {
		t.Fatalf("decoded %d frames of %d", len(decoded), n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the folder holds %v (%v), want the video alone", entries, err)
	}
}

// INF-VE1: with Software false the writer may take the hardware encoder; the
// Encoded says which ran. A few frames under the time limit, RGBA input. It
// runs the GPU's encoder, so it runs only when OA_VIDEOENC_HARDWARE=1 is set
// for a supervised run: one at a time, out of Modern Standby, with the
// System log read for display events before and after (owner rule on GPU
// work, 2026-10-01).
func TestHardwareEncoderWhenPresent(t *testing.T) {
	if os.Getenv("OA_VIDEOENC_HARDWARE") != "1" {
		t.Skip("a hardware encoder run is supervised: set OA_VIDEOENC_HARDWARE=1")
	}
	const n = 8
	path := filepath.Join(t.TempDir(), "hardware.mp4")
	sources := testFrames(n, RGBA)
	r := encodeWithin(t, path, Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGBA}, sources)
	t.Logf("encoder: %s (hardware %v)", r.EncoderName, r.Hardware)
	checkMP4(t, path, n, true)
	decoded := decodeFrames(t, path)
	if len(decoded) != n {
		t.Fatalf("decoded %d frames of %d", len(decoded), n)
	}
	rgb := make([]byte, 0, testW*testH*3)
	for i := 0; i < len(sources[3]); i += 4 {
		rgb = append(rgb, sources[3][i:i+3]...)
	}
	if got := psnr(decoded[3], rgb); got < minPSNR {
		t.Fatalf("frame 3: PSNR %.2f dB under %.0f", got, minPSNR)
	} else {
		t.Logf("frame 3: PSNR %.2f dB", got)
	}
}

// Encode returns the MP4's bytes and leaves no file behind; a frame of the
// wrong size and a video of no frame are refused, and an aborted encoder
// leaves nothing.
func TestEncodeBytesAndRefusals(t *testing.T) {
	dir := t.TempDir()
	o := Options{Width: testW, Height: testH, FPS: testFPS, Layout: RGB24, Software: true}
	data, r, err := Encode(dir, o, testFrames(3, RGB24))
	if errors.Is(err, ErrNoSystemEncoder) {
		t.Skip(err)
	}
	if err != nil || r.Frames != 3 || len(data) < 100 || string(data[4:8]) != "ftyp" {
		t.Fatalf("Encode: %d bytes, %+v, %v", len(data), r, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("Encode left %v", entries)
	}
	path := filepath.Join(dir, "short.mp4")
	e, err := Create(path, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Write(make([]byte, 10)); !errors.Is(err, ErrOptions) {
		t.Fatalf("a short frame: %v", err)
	}
	if _, err := e.Close(); !errors.Is(err, ErrOptions) {
		t.Fatalf("no frame: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty video was kept: %v", err)
	}
	e, err = Create(path, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Write(frame(0, testW, testH, RGB24)); err != nil {
		t.Fatal(err)
	}
	e.Abort()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an aborted video was kept: %v", err)
	}
	if err := e.Write(frame(0, testW, testH, RGB24)); err == nil {
		t.Fatal("an aborted encoder took a frame")
	}
}
