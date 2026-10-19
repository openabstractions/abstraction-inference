//go:build windows

package videoenc

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OA's own binding of the Media Foundation calls this package makes:
// mfplat.dll, mfreadwrite.dll and ole32.dll through x/sys/windows, and COM
// methods by their vtable slot. Slots and GUIDs are those of the Windows SDK
// 10.0.26100.0 headers mfobjects.h, mfidl.h, mfreadwrite.h, mftransform.h
// and mfapi.h.

var (
	mfplat      = windows.NewLazySystemDLL("mfplat.dll")
	mfreadwrite = windows.NewLazySystemDLL("mfreadwrite.dll")
	ole32       = windows.NewLazySystemDLL("ole32.dll")

	procMFStartup                   = mfplat.NewProc("MFStartup")
	procMFShutdown                  = mfplat.NewProc("MFShutdown")
	procMFCreateAttributes          = mfplat.NewProc("MFCreateAttributes")
	procMFCreateMediaType           = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample              = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer        = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFCreateSinkWriterFromURL   = mfreadwrite.NewProc("MFCreateSinkWriterFromURL")
	procMFCreateSourceReaderFromURL = mfreadwrite.NewProc("MFCreateSourceReaderFromURL")
	procCoTaskMemFree               = ole32.NewProc("CoTaskMemFree")
)

// mfVersion is MF_VERSION: MF_SDK_VERSION 2 and MF_API_VERSION 0x70.
const mfVersion = 0x00020070

func guid(d1 uint32, d2, d3 uint16, d4 ...byte) windows.GUID {
	g := windows.GUID{Data1: d1, Data2: d2, Data3: d3}
	copy(g.Data4[:], d4)
	return g
}

// mediaTypeGUID is DEFINE_MEDIATYPE_GUID: a FOURCC or D3DFMT in the base
// GUID XXXXXXXX-0000-0010-8000-00AA00389B71.
func mediaTypeGUID(code uint32) windows.GUID {
	return guid(code, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
}

var (
	mfMTMajorType          = guid(0x48eba18e, 0xf8c9, 0x4687, 0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f)
	mfMTSubtype            = guid(0xf7e34c9a, 0x42e8, 0x4714, 0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5)
	mfMTAvgBitrate         = guid(0x20332624, 0xfb0d, 0x4d9e, 0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e)
	mfMTInterlaceMode      = guid(0xe2724bb8, 0xe676, 0x4806, 0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd)
	mfMTFrameSize          = guid(0x1652c33d, 0xd6b2, 0x4012, 0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d)
	mfMTFrameRate          = guid(0xc459a2e8, 0x3d2c, 0x4e44, 0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0)
	mfMTPixelAspectRatio   = guid(0xc6376a1e, 0x8d0a, 0x4027, 0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6)
	mfMTDefaultStride      = guid(0x644b4e48, 0x1e02, 0x4516, 0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6)
	mfMTMPEG2Profile       = guid(0xad76a80b, 0x2d5c, 0x4e0b, 0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36)
	mfMediaTypeVideo       = guid(0x73646976, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
	mfVideoFormatH264      = mediaTypeGUID('H' | '2'<<8 | '6'<<16 | '4'<<24)
	mfVideoFormatRGB32     = mediaTypeGUID(22) // D3DFMT_X8R8G8B8
	mfReadwriteEnableHWMFT = guid(0xa634a91c, 0x822b, 0x41b9, 0xa4, 0x94, 0x4d, 0xe4, 0x64, 0x36, 0x12, 0xb0)
	mfTranscodeContainer   = guid(0x150ff23f, 0x4abc, 0x478b, 0xac, 0x4f, 0xe1, 0x91, 0x6f, 0xba, 0x1c, 0xca)
	mfContainerMPEG4       = guid(0xdc6cd05d, 0xb9d0, 0x40ef, 0xbd, 0x35, 0xfa, 0x62, 0x2c, 0x1a, 0xb2, 0x8a)
	mfSinkNoThrottling     = guid(0x08b845d8, 0x2b74, 0x4afe, 0x9d, 0x53, 0xbe, 0x16, 0xd2, 0xd5, 0xae, 0x4f)
	mfReaderVideoProcess   = guid(0xfb394f3d, 0xccf1, 0x42ee, 0xbb, 0xb3, 0xf9, 0xb8, 0x45, 0xd5, 0x68, 0x1d)
	mftCategoryVideoEnc    = guid(0xf79eac7d, 0xe545, 0x4387, 0xbd, 0xee, 0xd6, 0x47, 0xd7, 0xbd, 0xe4, 0x2a)
	mftHardwareURL         = guid(0x2fb866ac, 0xb078, 0x4942, 0xab, 0x6c, 0x00, 0x3d, 0x05, 0xcd, 0xa6, 0x74)
	mftFriendlyName        = guid(0x314ffbae, 0x5b41, 0x4c95, 0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3)
	iidIMFSinkWriterEx     = guid(0x588d72ab, 0x5bc1, 0x496a, 0x87, 0x14, 0xb7, 0x06, 0x17, 0x14, 0x1b, 0x25)
	iidIMF2DBuffer         = guid(0x7dc9d5f9, 0x9ed9, 0x44ec, 0x9b, 0xbf, 0x06, 0x00, 0xbb, 0x58, 0x9f, 0xbb)
)

// Vtable slots. IUnknown: 0 QueryInterface, 2 Release.
const (
	slotQueryInterface = 0
	slotRelease        = 2

	// IMFAttributes (and IMFMediaType, IMFSample, which derive from it).
	slotGetUINT32          = 7
	slotGetUINT64          = 8
	slotGetStringLength    = 11
	slotGetAllocatedString = 13
	slotSetUINT32          = 21
	slotSetUINT64          = 22
	slotSetGUID            = 24

	// IMFSample.
	slotSetSampleTime             = 36
	slotSetSampleDuration         = 38
	slotConvertToContiguousBuffer = 41
	slotAddBuffer                 = 42

	// IMFMediaBuffer.
	slotLock             = 3
	slotUnlock           = 4
	slotSetCurrentLength = 6

	// IMF2DBuffer.
	slotLock2D   = 3
	slotUnlock2D = 4

	// IMFSinkWriter and IMFSinkWriterEx.
	slotAddStream             = 3
	slotSetInputMediaType     = 4
	slotBeginWriting          = 5
	slotWriteSample           = 6
	slotFinalize              = 11
	slotGetTransformForStream = 14

	// IMFTransform.
	slotGetAttributes = 8

	// IMFSourceReader.
	slotGetCurrentMediaType = 6
	slotSetCurrentMediaType = 7
	slotReadSample          = 9
)

// Values the binding passes.
const (
	interlaceProgressive     = 2          // MFVideoInterlace_Progressive
	h264ProfileHigh          = 100        // eAVEncH264VProfile_High
	firstVideoStream         = 0xFFFFFFFC // MF_SOURCE_READER_FIRST_VIDEO_STREAM
	readerEndOfStream        = 0x2        // MF_SOURCE_READERF_ENDOFSTREAM
	mfInvalidIndex           = 0xC00D36BF // MF_E_INVALIDINDEX
	mfAttributeNotFound      = 0xC00D36E6 // MF_E_ATTRIBUTENOTFOUND
	hresultNotImplemented    = 0x80004001 // E_NOTIMPL
	hresultChangedMode       = 0x80010106 // RPC_E_CHANGED_MODE
	readerAllStreams         = 0xFFFFFFFE // MF_SOURCE_READER_ALL_STREAMS
	readerFirstVideoSelected = 1
)

// comObject is any COM interface pointer: a pointer to its vtable. The
// object lives in COM's memory, never Go's.
type comObject struct{ vtbl *[64]uintptr }

// call invokes vtable slot with the object as the first argument and returns
// the HRESULT. uintptrescapes keeps every pointer converted to a uintptr in
// args on the heap and alive for the call, as x/sys/windows' Proc.Call does.
//
//go:uintptrescapes
func (o *comObject) call(slot int, args ...uintptr) uintptr {
	//unchecked: SyscallN's raw GetLastError is not meaningful for a COM vtable call; every caller inspects the returned HRESULT instead
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

// release drops one reference; a nil object is nothing.
func (o *comObject) release() {
	if o != nil {
		o.call(slotRelease)
	}
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// hresult is the error of a failed call, naming what was called.
type hresult struct {
	what string
	code uint32
}

func (h *hresult) Error() string { return fmt.Sprintf("videoenc: %s: HRESULT 0x%08X", h.what, h.code) }

func check(what string, hr uintptr) error {
	if failed(hr) {
		return &hresult{what: what, code: uint32(hr)}
	}
	return nil
}

// codeOf is a failure's HRESULT, or 0.
func codeOf(err error) uint32 {
	var h *hresult
	if errors.As(err, &h) {
		return h.code
	}
	return 0
}

// proc calls an exported function that returns an HRESULT.
//
//go:uintptrescapes
func proc(p *windows.LazyProc, what string, args ...uintptr) error {
	if err := p.Find(); err != nil {
		return fmt.Errorf("%w: %v", ErrNoSystemEncoder, err)
	}
	//unchecked: the HRESULT primary return is checked below; Call's raw GetLastError third field carries no separate information for an HRESULT call
	hr, _, _ := p.Call(args...)
	return check(what, hr)
}

// attributes is a new attribute store of room n.
func newAttributes(n int) (*comObject, error) {
	var a *comObject
	if err := proc(procMFCreateAttributes, "MFCreateAttributes", uintptr(unsafe.Pointer(&a)), uintptr(n)); err != nil {
		return nil, err
	}
	return a, nil
}

func newMediaType() (*comObject, error) {
	var t *comObject
	if err := proc(procMFCreateMediaType, "MFCreateMediaType", uintptr(unsafe.Pointer(&t))); err != nil {
		return nil, err
	}
	return t, nil
}

func (o *comObject) setGUID(key, value *windows.GUID) error {
	return check("IMFAttributes::SetGUID", o.call(slotSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(value))))
}

func (o *comObject) setUINT32(key *windows.GUID, value uint32) error {
	return check("IMFAttributes::SetUINT32", o.call(slotSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(value)))
}

func (o *comObject) setUINT64(key *windows.GUID, value uint64) error {
	return check("IMFAttributes::SetUINT64", o.call(slotSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(value)))
}

// setPair writes two 32-bit halves as one UINT64, as MFSetAttributeSize and
// MFSetAttributeRatio do.
func (o *comObject) setPair(key *windows.GUID, high, low uint32) error {
	return o.setUINT64(key, uint64(high)<<32|uint64(low))
}

// videoType is a video media type of subtype with the stream's size, rate,
// square pixels and progressive frames.
func videoType(subtype *windows.GUID, o Options) (*comObject, error) {
	t, err := newMediaType()
	if err != nil {
		return nil, err
	}
	err = errors.Join(
		t.setGUID(&mfMTMajorType, &mfMediaTypeVideo),
		t.setGUID(&mfMTSubtype, subtype),
		t.setUINT32(&mfMTInterlaceMode, interlaceProgressive),
		t.setPair(&mfMTFrameSize, uint32(o.Width), uint32(o.Height)),
		t.setPair(&mfMTFrameRate, uint32(o.FPS), 1),
		t.setPair(&mfMTPixelAspectRatio, 1, 1),
	)
	if err != nil {
		t.release()
		return nil, err
	}
	return t, nil
}

// startup enters this OS thread into the multithreaded apartment and starts
// Media Foundation; stop undoes both. The caller holds the thread locked.
func startup() (stop func(), err error) {
	if err := mfplat.Load(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSystemEncoder, err)
	}
	if err := mfreadwrite.Load(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoSystemEncoder, err)
	}
	// S_FALSE (1), a thread already in the apartment, still takes one
	// CoUninitialize; RPC_E_CHANGED_MODE, a thread in another apartment,
	// takes none and fails Media Foundation's calls below.
	co := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
	var errno syscall.Errno
	uninit := co == nil || errors.As(co, &errno) && errno == 1
	if co != nil && !uninit {
		return nil, fmt.Errorf("videoenc: CoInitializeEx: %w", co)
	}
	if err := proc(procMFStartup, "MFStartup", mfVersion, 0); err != nil {
		if uninit {
			windows.CoUninitialize()
		}
		return nil, err
	}
	return func() {
		//unchecked: shutting Media Foundation down on this thread; nothing is left to report a failure to
		procMFShutdown.Call()
		if uninit {
			windows.CoUninitialize()
		}
	}, nil
}

// utf16 is s as a NUL-terminated UTF-16 string on the heap.
func utf16(s string) (*uint16, error) { return windows.UTF16PtrFromString(s) }

// coString takes a CoTaskMem string, copies it and frees it.
func coString(p *uint16) string {
	if p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	//unchecked: freeing a COM-allocated string; CoTaskMemFree returns nothing
	procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}
