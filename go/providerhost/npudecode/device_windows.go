package npudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/openabstractions/abstraction-identity/go/trustedpath"
	"github.com/openabstractions/abstraction-inference/go/providerhost/modelpack"

	identity "github.com/openabstractions/abstraction-identity"
)

// shim is the loaded oa_npu_decode.dll. It stays loaded for the life of
// the process: XRT keeps threads of its own in the driver's
// xrt_coreutil.dll, which unloading the shim could unload under them.
type shim struct {
	query, open, decode, close uintptr
}

var (
	shimMu     sync.Mutex
	shimLoaded = map[string]*shim{}
)

// readLocked opens path denying writes and deletes to everyone, checks
// that the handle's final path is path itself, reads the file whole, and
// returns the open file: while it stays open the bytes on disk are the
// bytes read.
func readLocked(path string) (*os.File, []byte, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrKit, path, err)
	}
	f := os.NewFile(uintptr(h), path)
	// A spelling check on purpose: path and the handle's final path are one
	// file by construction, and the question is whether anything on the way
	// (a junction, a mount point, an alias) spells it differently, which only
	// the names answer.
	if final, err := finalPath(h); err != nil || !identity.SamePathName(final, path) {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s resolves to %q (%v)", ErrKit, path, final, err)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrKit, path, err)
	}
	return f, b, nil
}

// finalPath is the path the open handle h names, without the \\?\ prefix;
// a mapped or substituted drive, or a share, resolves to another path.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`), nil
		}
		buf = make([]uint16, n+1)
	}
}

// kit is a kit folder's files, checked against its manifest.
type kit struct {
	s                    *shim
	xclbin, insts, shape []byte
}

// kitOwners are the principals a kit file may be written by: the machine's
// owners (SYSTEM, Administrators, TrustedInstaller), so an installer's kit
// under Program Files passes, and this account, for a kit OA built.
func kitOwners() trustedpath.Owners {
	o := trustedpath.MachineOwners()
	o.Account = true
	return o
}

// loadKit checks cfg's kit and loads its shim. XRT must find no
// configuration of the process's own (checkXRTConfig). Every kit file,
// kit.json included, must pass identity's ID-D2 (trustedpath.CheckFile)
// with kitOwners: it and every folder above it owned by them, no other
// principal able to change or replace it, no reparse point on its path. It
// is then read through a handle that denies writes and whose final path is
// its own (no mapped or substituted drive), and must have the SHA-256
// kit.json pins. The shim is loaded while its handle is open, so the
// library loaded is the one checked; its own imports come from System32
// only.
func loadKit(cfg Config) (*kit, error) {
	if err := checkXRTConfig(); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	for _, name := range append([]string{ManifestFile}, kitFiles...) {
		if err := trustedpath.CheckFile(filepath.Join(dir, name), kitOwners()); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrKit, err)
		}
	}
	mf, raw, err := readLocked(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	mf.Close()
	m, err := parseManifest(raw, cfg.ManifestSHA256)
	if err != nil {
		return nil, err
	}
	k := &kit{}
	var shimFile *os.File
	for _, name := range kitFiles {
		f, b, err := readLocked(filepath.Join(dir, name))
		if err != nil {
			if shimFile != nil {
				shimFile.Close()
			}
			return nil, err
		}
		if err := m.checkDigest(name, b); err != nil {
			f.Close()
			if shimFile != nil {
				shimFile.Close()
			}
			return nil, err
		}
		switch name {
		case ShimFile:
			shimFile = f
			continue
		case XclbinFile:
			k.xclbin = b
		case InstsFile:
			k.insts = b
		case ShapeFile:
			k.shape = b
		}
		f.Close()
	}
	defer shimFile.Close()
	k.s, err = loadShim(filepath.Join(dir, ShimFile))
	return k, err
}

// loadShim loads the shim at path, an absolute path, once per process,
// resolving its imports (xrt_coreutil.dll and the MSVC runtime) from
// System32 alone, so no DLL in the kit folder, the working directory or on
// PATH is loaded with it.
func loadShim(path string) (*shim, error) {
	shimMu.Lock()
	defer shimMu.Unlock()
	if s, ok := shimLoaded[path]; ok {
		return s, nil
	}
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return nil, fmt.Errorf("npudecode: load %s: %w", path, err)
	}
	s := &shim{}
	for _, p := range []struct {
		name string
		addr *uintptr
	}{{"oa_npu_query", &s.query}, {"oa_npu_open", &s.open}, {"oa_npu_decode", &s.decode}, {"oa_npu_close", &s.close}} {
		if *p.addr, err = windows.GetProcAddress(h, p.name); err != nil {
			windows.FreeLibrary(h)
			return nil, fmt.Errorf("%w: %s exports no %s: %v", ErrKit, path, p.name, err)
		}
	}
	shimLoaded[path] = s
	return s, nil
}

const errLen = 512

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// mapped is the n bytes at a mapped XRT buffer's address, outside Go's
// heap and valid until the device is closed.
func mapped(addr uintptr, n int) []byte {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return unsafe.Slice((*byte)(p), n)
}

// Device is the NPU decoder of one kit. It is a modelpack.FixedDevice.
type Device struct {
	s       *shim
	h       uintptr
	shape   Shape
	in, out [][]byte
	timeout time.Duration
	info    string
	// free releases the native handle; Close calls it once.
	free func()

	mu sync.Mutex // one run at a time
	// claim holds a token while a fill or the self-test uses the device;
	// Close takes it for good, so it frees nothing a fill still uses.
	claim     chan struct{}
	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
	err       atomic.Pointer[error]
	hung      atomic.Bool
	runs      atomic.Int64
	runNS     atomic.Int64
}

func query(s *shim) (string, error) {
	var info, msg [errLen]byte
	r, _, _ := syscall.SyscallN(s.query, uintptr(unsafe.Pointer(&info[0])), errLen, uintptr(unsafe.Pointer(&msg[0])), errLen)
	if int32(r) != 0 {
		return "", fmt.Errorf("npudecode: %s", cString(msg[:]))
	}
	return cString(info[:]), nil
}

// Query checks and loads the kit as Open does and names the NPU XRT
// finds, or reports why there is none; it opens no hardware context and
// runs nothing.
func Query(cfg Config) (string, error) {
	k, err := loadKit(cfg)
	if err != nil {
		return "", err
	}
	return query(k.s)
}

// Open checks and loads the kit in cfg.Dir (loadKit), opens the NPU with
// its design and runs modelpack.CheckFixedDevice. It returns the device
// only when the self-test decoded every slot of both buffer sets
// bit-exact; on any error the device is closed.
func Open(ctx context.Context, cfg Config) (*Device, error) {
	k, err := loadKit(cfg)
	if err != nil {
		return nil, err
	}
	shape, err := parseShape(k.shape)
	if err != nil {
		return nil, err
	}
	if len(k.xclbin) == 0 || len(k.insts) == 0 || len(k.insts)%4 != 0 {
		return nil, fmt.Errorf("%w: %s of %d bytes, %s of %d", ErrKit, XclbinFile, len(k.xclbin), InstsFile, len(k.insts))
	}
	name, err := query(k.s)
	if err != nil {
		return nil, err
	}
	kernel, err := windows.BytePtrFromString(shape.Kernel)
	if err != nil {
		return nil, fmt.Errorf("%w: kernel name %q", ErrKit, shape.Kernel)
	}
	inBytes, outBytes := shape.Units*modelpack.FixedSlot, shape.Units*modelpack.ChunkBytes
	var h uintptr
	var ins, outs [Sets]uintptr
	var msg [errLen]byte
	r, _, _ := syscall.SyscallN(k.s.open,
		uintptr(unsafe.Pointer(&k.xclbin[0])), uintptr(len(k.xclbin)),
		uintptr(unsafe.Pointer(&k.insts[0])), uintptr(len(k.insts)),
		uintptr(unsafe.Pointer(kernel)), uintptr(inBytes), uintptr(outBytes), Sets,
		uintptr(unsafe.Pointer(&h)), uintptr(unsafe.Pointer(&ins[0])), uintptr(unsafe.Pointer(&outs[0])),
		uintptr(unsafe.Pointer(&msg[0])), errLen)
	if int32(r) != 0 {
		return nil, fmt.Errorf("npudecode: %s", cString(msg[:]))
	}
	s := k.s
	d := newDevice(shape, cfg.Timeout, name, func() { syscall.SyscallN(s.close, h) })
	d.s, d.h = s, h
	for set := range Sets {
		d.in = append(d.in, mapped(ins[set], inBytes))
		d.out = append(d.out, mapped(outs[set], outBytes))
	}
	if err := modelpack.CheckFixedDevice(ctx, d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// newDevice is a device whose native handle free releases.
func newDevice(shape Shape, timeout time.Duration, info string, free func()) *Device {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Device{shape: shape, timeout: timeout, info: info, free: free, claim: make(chan struct{}, 1)}
}

// Info names the NPU.
func (d *Device) Info() string { return d.info }

// Shape is the design's batch.
func (d *Device) Shape() Shape { return d.shape }

// Stats reports the runs so far and their time from start to completion.
func (d *Device) Stats() (int, time.Duration) {
	return int(d.runs.Load()), time.Duration(d.runNS.Load())
}

func (d *Device) Slots() int         { return d.shape.Units }
func (d *Device) Sets() int          { return Sets }
func (d *Device) In(set int) []byte  { return d.in[set] }
func (d *Device) Out(set int) []byte { return d.out[set] }

// TryAcquire claims the device for one fill; it reports false while
// another fill holds it and after Close.
func (d *Device) TryAcquire() bool {
	select {
	case d.claim <- struct{}{}:
		return true
	default:
		return false
	}
}

// Done gives back the claim TryAcquire took.
func (d *Device) Done() { <-d.claim }

func (d *Device) Err() error {
	if e := d.err.Load(); e != nil {
		return *e
	}
	return nil
}

func (d *Device) Distrust(err error) { d.err.CompareAndSwap(nil, &err) }

// Decode runs the design once on buffer set `set` and waits at most the
// device's timeout. The first error or timeout is kept: the device then
// refuses every later run.
func (d *Device) Decode(ctx context.Context, set int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.Err(); err != nil {
		return err
	}
	if d.closed.Load() {
		return errors.New("npudecode: the device is closed")
	}
	if set < 0 || set >= Sets {
		return fmt.Errorf("npudecode: no buffer set %d", set)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ms := max(1, d.timeout.Milliseconds())
	var ns uint64
	var msg [errLen]byte
	r, _, _ := syscall.SyscallN(d.s.decode, d.h, uintptr(set), uintptr(ms), uintptr(unsafe.Pointer(&ns)), uintptr(unsafe.Pointer(&msg[0])), errLen)
	if int32(r) == 0 {
		d.runs.Add(1)
		d.runNS.Add(int64(ns))
		return nil
	}
	if int32(r) == -2 {
		d.hung.Store(true)
	}
	err := fmt.Errorf("npudecode: %s", cString(msg[:]))
	d.err.CompareAndSwap(nil, &err)
	return err
}

// Close waits until no fill holds the device, takes the claim for good,
// and frees the device's buffers, kernel and hardware context. After a
// timed-out run it frees nothing: the run may still own the buffers, and
// the process's exit releases them. A second Close returns the first's
// result.
func (d *Device) Close() error {
	d.closeOnce.Do(func() {
		d.claim <- struct{}{} // waits for Done; never given back
		d.mu.Lock()
		defer d.mu.Unlock()
		d.closed.Store(true)
		if d.hung.Load() {
			d.closeErr = errors.New("npudecode: a run timed out; its buffers stay allocated until the process exits")
			return
		}
		d.in, d.out = nil, nil
		if d.free != nil {
			d.free()
		}
	})
	return d.closeErr
}

var _ modelpack.FixedDevice = (*Device)(nil)
