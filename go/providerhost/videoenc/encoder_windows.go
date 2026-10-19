//go:build windows

package videoenc

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/openabstractions/abstraction-inference/go/providerhost/noreplace"
)

// errEnded is a call on an encoder that was closed or aborted.
var errEnded = errors.New("videoenc: the encoder has ended")

// An Encoder writes one MP4 frame by frame. Media Foundation's objects live
// on one locked OS thread in the multithreaded apartment for the encoder's
// whole life; Write and Close hand their work to it. Write is safe for
// concurrent use: frames are ordered by their arrival on that thread.
type Encoder struct {
	path, temp string
	opts       Options

	// calls carries work to the encoder's thread and is never closed; quit
	// tells the thread to end, and done closes once it has.
	calls    chan func()
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu    sync.Mutex
	ended bool

	// Owned by the encoder's thread.
	writer  *comObject
	stream  uint32
	pixels  []byte
	written int
	result  Encoded
}

// Create starts an MP4 for path, which must be an absolute .mp4 path in an
// existing folder where no file stands yet. The writer writes a temporary
// name in the same folder, and Close moves it to path without replacing
// anything there (noreplace). The H.264 encoder is chosen when the writer
// starts: the hardware encoder when one is present and Options.Software is
// false, Microsoft's software encoder otherwise.
func Create(path string, o Options) (*Encoder, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if err := checkPath(path); err != nil {
		return nil, err
	}
	if o.Bitrate == 0 {
		o.Bitrate = DefaultBitrate(o.Width, o.Height, o.FPS)
	}
	temp, err := tempName(path)
	if err != nil {
		return nil, err
	}
	e := &Encoder{path: path, temp: temp, opts: o, calls: make(chan func()), quit: make(chan struct{}),
		done: make(chan struct{}), pixels: make([]byte, o.Width*o.Height*4)}
	started := make(chan error, 1)
	go e.own(started)
	if err := <-started; err != nil {
		<-e.done
		//unchecked: a writer that never began leaves at most an empty temporary file, removed here
		os.Remove(temp)
		return nil, err
	}
	return e, nil
}

// own is the encoder's thread: it starts Media Foundation and the writer,
// runs every call handed to it until quit, and then releases the writer,
// which closes the file, and stops Media Foundation.
func (e *Encoder) own(started chan<- error) {
	defer close(e.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	stop, err := startup()
	if err != nil {
		started <- err
		return
	}
	defer stop()
	defer func() {
		e.writer.release()
		e.writer = nil
	}()
	if err := e.begin(); err != nil {
		started <- err
		return
	}
	started <- nil
	for {
		select {
		case call := <-e.calls:
			call()
		case <-e.quit:
			return
		}
	}
}

// do runs f on the encoder's thread and returns its error; an encoder whose
// thread has ended answers errEnded. calls is never closed, so a sender is
// never left sending on a closed channel.
func (e *Encoder) do(f func() error) error {
	result := make(chan error, 1)
	select {
	case e.calls <- func() { result <- f() }:
	case <-e.done:
		return errEnded
	}
	return <-result
}

// begin makes the sink writer, its H.264 output stream and its RGB32 input,
// and starts writing.
func (e *Encoder) begin() error {
	attributes, err := newAttributes(4)
	if err != nil {
		return err
	}
	defer attributes.release()
	if err := errors.Join(
		attributes.setGUID(&mfTranscodeContainer, &mfContainerMPEG4),
		attributes.setUINT32(&mfSinkNoThrottling, 1),
		attributes.setUINT32(&mfReadwriteEnableHWMFT, boolValue(!e.opts.Software)),
	); err != nil {
		return err
	}
	url, err := utf16(e.temp)
	if err != nil {
		return err
	}
	var writer *comObject
	if err := proc(procMFCreateSinkWriterFromURL, "MFCreateSinkWriterFromURL", uintptr(unsafe.Pointer(url)), 0,
		uintptr(unsafe.Pointer(attributes)), uintptr(unsafe.Pointer(&writer))); err != nil {
		return err
	}
	e.writer = writer
	out, err := videoType(&mfVideoFormatH264, e.opts)
	if err != nil {
		return err
	}
	defer out.release()
	if err := errors.Join(
		out.setUINT32(&mfMTAvgBitrate, uint32(e.opts.Bitrate)),
		out.setUINT32(&mfMTMPEG2Profile, h264ProfileHigh),
	); err != nil {
		return err
	}
	if err := check("IMFSinkWriter::AddStream", writer.call(slotAddStream, uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(&e.stream)))); err != nil {
		return err
	}
	in, err := videoType(&mfVideoFormatRGB32, e.opts)
	if err != nil {
		return err
	}
	defer in.release()
	// A positive stride: rows top to bottom, as the frames arrive.
	if err := in.setUINT32(&mfMTDefaultStride, uint32(e.opts.Width*4)); err != nil {
		return err
	}
	if err := check("IMFSinkWriter::SetInputMediaType", writer.call(slotSetInputMediaType, uintptr(e.stream), uintptr(unsafe.Pointer(in)), 0)); err != nil {
		return err
	}
	if err := check("IMFSinkWriter::BeginWriting", writer.call(slotBeginWriting)); err != nil {
		return err
	}
	e.result = Encoded{Path: e.path}
	e.result.Hardware, e.result.EncoderName = e.which()
	return nil
}

func boolValue(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// which names the H.264 encoder the writer chose for the stream: a
// transform of the video encoder category that names a hardware URL is a
// hardware encoder.
func (e *Encoder) which() (bool, string) {
	var ex *comObject
	if failed(e.writer.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidIMFSinkWriterEx)), uintptr(unsafe.Pointer(&ex)))) || ex == nil {
		return false, "software H.264 encoder"
	}
	defer ex.release()
	for index := uintptr(0); index < 16; index++ {
		var category windows.GUID
		var transform *comObject
		hr := ex.call(slotGetTransformForStream, uintptr(e.stream), index, uintptr(unsafe.Pointer(&category)), uintptr(unsafe.Pointer(&transform)))
		if failed(hr) {
			break
		}
		if category != mftCategoryVideoEnc {
			transform.release()
			continue
		}
		hardware, name := describe(transform)
		transform.release()
		return hardware, name
	}
	return false, "software H.264 encoder"
}

// describe reads an encoder transform's hardware URL and friendly name.
func describe(transform *comObject) (bool, string) {
	var attributes *comObject
	if failed(transform.call(slotGetAttributes, uintptr(unsafe.Pointer(&attributes)))) || attributes == nil {
		return false, "software H.264 encoder"
	}
	defer attributes.release()
	var length uint32
	hardware := !failed(attributes.call(slotGetStringLength, uintptr(unsafe.Pointer(&mftHardwareURL)), uintptr(unsafe.Pointer(&length))))
	var text *uint16
	var chars uint32
	name := ""
	if !failed(attributes.call(slotGetAllocatedString, uintptr(unsafe.Pointer(&mftFriendlyName)), uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&chars)))) {
		name = coString(text)
	}
	if name == "" {
		name = "software H.264 encoder"
		if hardware {
			name = "hardware H.264 encoder"
		}
	}
	return hardware, name
}

// Write adds one frame of Options.Width x Height pixels in Options.Layout.
// It is safe for concurrent use: each frame takes the next index on the
// encoder's thread, in the order the frames arrive there.
func (e *Encoder) Write(frame []byte) error {
	if len(frame) != e.opts.frameBytes() {
		return fmt.Errorf("%w: a frame of %d bytes is not %dx%d in its format (%d bytes)", ErrOptions, len(frame), e.opts.Width, e.opts.Height, e.opts.frameBytes())
	}
	e.mu.Lock()
	ended := e.ended
	e.mu.Unlock()
	if ended {
		return errEnded
	}
	return e.do(func() error {
		bgrx(e.pixels, frame, e.opts)
		if err := e.writeSample(e.written); err != nil {
			return err
		}
		e.written++
		return nil
	})
}

// writeSample hands frame index, already in pixels, to the writer.
func (e *Encoder) writeSample(index int) error {
	var buffer *comObject
	if err := proc(procMFCreateMemoryBuffer, "MFCreateMemoryBuffer", uintptr(len(e.pixels)), uintptr(unsafe.Pointer(&buffer))); err != nil {
		return err
	}
	defer buffer.release()
	var data *byte
	if err := check("IMFMediaBuffer::Lock", buffer.call(slotLock, uintptr(unsafe.Pointer(&data)), 0, 0)); err != nil {
		return err
	}
	copy(unsafe.Slice(data, len(e.pixels)), e.pixels)
	if err := errors.Join(
		check("IMFMediaBuffer::Unlock", buffer.call(slotUnlock)),
		check("IMFMediaBuffer::SetCurrentLength", buffer.call(slotSetCurrentLength, uintptr(len(e.pixels)))),
	); err != nil {
		return err
	}
	var sample *comObject
	if err := proc(procMFCreateSample, "MFCreateSample", uintptr(unsafe.Pointer(&sample))); err != nil {
		return err
	}
	defer sample.release()
	start, next := frameTime(index, e.opts.FPS), frameTime(index+1, e.opts.FPS)
	return errors.Join(
		check("IMFSample::AddBuffer", sample.call(slotAddBuffer, uintptr(unsafe.Pointer(buffer)))),
		check("IMFSample::SetSampleTime", sample.call(slotSetSampleTime, uintptr(start))),
		check("IMFSample::SetSampleDuration", sample.call(slotSetSampleDuration, uintptr(next-start))),
		check("IMFSinkWriter::WriteSample", e.writer.call(slotWriteSample, uintptr(e.stream), uintptr(unsafe.Pointer(sample)))),
	)
}

// fastStartFile is fastStart; only tests replace it.
var fastStartFile = fastStart

// Close finishes the MP4, moves it from its temporary name to the path
// Create named without replacing a file that appeared there since, and
// returns what it holds. A video with no frame, a writer that cannot finish
// and a path taken in the meantime are failures that leave no file of
// theirs. A finished video is never discarded for its layout: when moving
// moov before mdat fails, the encoder's own file is placed, moov last, and
// the result reads FastStart false.
func (e *Encoder) Close() (Encoded, error) {
	e.mu.Lock()
	if e.ended {
		e.mu.Unlock()
		return Encoded{}, errEnded
	}
	e.ended = true
	e.mu.Unlock()
	frames := 0
	err := e.do(func() error {
		frames = e.written
		if frames == 0 {
			return fmt.Errorf("%w: a video needs at least one frame", ErrOptions)
		}
		return check("IMFSinkWriter::Finalize", e.writer.call(slotFinalize))
	})
	result := e.result
	// The writer's release on the thread closes the temporary file.
	e.stop()
	if err != nil {
		//unchecked: a writer that could not finish leaves no video
		os.Remove(e.temp)
		return Encoded{}, err
	}
	// Fast start: moov moves before mdat in a second temporary file, which
	// then takes the path without replacing anything there.
	fast := e.temp + ".fast.mp4"
	placed, spare := e.temp, fast
	if fastStartFile(e.temp, fast) == nil {
		placed, spare = fast, e.temp
		result.FastStart = true
	}
	//unchecked: the file that is not placed is not kept: the encoder's own once moved, or a partial fast-start copy
	os.Remove(spare)
	if err := noreplace.RenameNew(placed, e.path); err != nil {
		//unchecked: a temporary file that did not become the video is not kept
		os.Remove(placed)
		return Encoded{}, err
	}
	result.Frames = frames
	result.Duration = time.Duration(frameTime(frames, e.opts.FPS) * 100)
	return result, nil
}

// Abort stops the encoder without finishing the MP4 and removes its
// temporary file. It is safe to call while Writes are in flight; they end
// with errEnded or with their frame written to the file that is removed.
func (e *Encoder) Abort() {
	e.mu.Lock()
	already := e.ended
	e.ended = true
	e.mu.Unlock()
	if already {
		return
	}
	e.stop()
	//unchecked: an aborted MP4 is not kept
	os.Remove(e.temp)
}

// stop tells the encoder's thread to end and waits until it has.
func (e *Encoder) stop() {
	e.stopOnce.Do(func() { close(e.quit) })
	<-e.done
}
