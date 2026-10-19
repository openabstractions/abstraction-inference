package modelpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeDevice decodes as the NPU kernel does (native/fixed3_decode.cc in
// package npudecode): each slot's 64 chunks, every value's exponent the
// window base plus its code, modulo 256, whatever the slot holds.
type fakeDevice struct {
	slots   int
	in, out [][]byte
	busy    atomic.Bool
	err     atomic.Pointer[error]
	decodes atomic.Int64
	failAt  int64 // the Decode, counted from 1, that fails
	flipAt  int64 // the Decode whose output gets one bit flipped
	running atomic.Int32
	overlap atomic.Bool // two Decodes ran at once
}

func newFakeDevice(slots, sets int) *fakeDevice {
	f := &fakeDevice{slots: slots}
	for range sets {
		f.in = append(f.in, make([]byte, slots*FixedSlot))
		f.out = append(f.out, make([]byte, slots*ChunkBytes))
	}
	return f
}

func (f *fakeDevice) Slots() int         { return f.slots }
func (f *fakeDevice) Sets() int          { return len(f.in) }
func (f *fakeDevice) In(set int) []byte  { return f.in[set] }
func (f *fakeDevice) Out(set int) []byte { return f.out[set] }
func (f *fakeDevice) TryAcquire() bool   { return f.busy.CompareAndSwap(false, true) }
func (f *fakeDevice) Done()              { f.busy.Store(false) }

func (f *fakeDevice) Err() error {
	if e := f.err.Load(); e != nil {
		return *e
	}
	return nil
}

func (f *fakeDevice) Distrust(err error) { f.err.CompareAndSwap(nil, &err) }

func (f *fakeDevice) Decode(ctx context.Context, set int) error {
	if err := f.Err(); err != nil {
		return err
	}
	if f.running.Add(1) > 1 {
		f.overlap.Store(true)
	}
	defer f.running.Add(-1)
	n := f.decodes.Add(1)
	if n == f.failAt {
		err := errors.New("fake device: run ended in state 7")
		f.err.CompareAndSwap(nil, &err)
		return err
	}
	for k := range f.slots {
		decodeSlotLikeTheKernel(f.out[set][k*ChunkBytes:][:ChunkBytes], f.in[set][k*FixedSlot:][:FixedSlot])
	}
	if n == f.flipAt {
		f.out[set][0] ^= 1
	}
	return nil
}

// decodeSlotLikeTheKernel is fixed3_decode.cc over the 64 chunks of slot.
func decodeSlotLikeTheKernel(out, slot []byte) {
	for c := range ChunkBytes / 2 / fixedChunkVals {
		in := slot[fixedHeaderLen+c*fixedChunkLen:][:fixedChunkLen]
		o := out[2*c*fixedChunkVals:]
		for v := range fixedChunkVals {
			blk, i, j := v/512, v/64%8, v%64
			t := fixedChunkHead + 64*blk + j
			code := in[t]>>i&1 | (in[t+fixedPlaneLen]>>i&1)<<1 | (in[t+2*fixedPlaneLen]>>i&1)<<2
			e := uint16(in[0] + code)
			s := uint16(in[fixedChunkHead+3*fixedPlaneLen+v])
			binary.LittleEndian.PutUint16(o[2*v:], s&0x80<<8|e<<7|s&0x7f)
		}
	}
}

func TestFixedSlotIsTheLargestUnit(t *testing.T) {
	if FixedSlot != maxPackedFixed(ChunkBytes) || FixedSlot != 397_376 {
		t.Fatalf("FixedSlot %d, maxPackedFixed %d", FixedSlot, maxPackedFixed(ChunkBytes))
	}
}

// fusedUnits packs fusedObject with the fixed code alone.
func fusedUnits(t *testing.T) ([]byte, Index, []byte) {
	t.Helper()
	src, tensors := fusedObject()
	index, packed := pack(t, src, tensors, fixedOnly)
	if encodings(index)[EncodingExpFixed3V1] < 10 {
		t.Fatalf("units per encoding %v", encodings(index))
	}
	return src, index, packed
}

// INF-MP10: scanFixed fills a unit's patch as the fused CPU decode does,
// the boundary split, the escapes before it and the inline escape
// included, without decoding.
func TestScanFixedMatchesTheFusedDecode(t *testing.T) {
	_, index, packed := fusedUnits(t)
	inlines := 0
	for _, c := range index.Chunks {
		if c.Encoding != EncodingExpFixed3V1 {
			continue
		}
		p := packed[c.PackedOffset:][:c.Packed]
		want := newFixedPatch(&c)
		slot := make([]byte, c.Length)
		if err := decodeFixed(slot, p, want); err != nil {
			t.Fatal(err)
		}
		got := newFixedPatch(&c)
		e, err := scanFixed(p, int(c.Length), got)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.bases, want.bases) || !bytes.Equal(got.escapes, want.escapes) ||
			got.split != want.split || got.before != want.before || got.inline != want.inline {
			t.Fatalf("unit at %d: scan %+v, decode %+v", c.Offset, got, want)
		}
		if want.inline >= 0 {
			inlines++
			exp := int(binary.LittleEndian.Uint16(slot[2*want.inline:])>>7) & 0xff
			if e < 0 || e != exp {
				t.Fatalf("unit at %d: inline escape %d, the decode wrote exponent %d", c.Offset, e, exp)
			}
		}
	}
	if inlines == 0 {
		t.Fatal("no unit had an escaped value split across the span boundary")
	}
}

// INF-MP10: scanFixed refuses exactly the payloads the decoder refuses.
func TestScanFixedRefusesWhatTheDecoderRefuses(t *testing.T) {
	src := weights(3*fixedChunkVals+77, 91, bf16)
	for i := 0; i < len(src); i += 2 * 211 {
		binary.LittleEndian.PutUint16(src[i:], 0x7fc1)
	}
	good := fixedRoundTrip(t, src)
	r := rand.New(rand.NewPCG(92, 93))
	refused := 0
	for n := range 3000 {
		p := bytes.Clone(good)
		at := r.IntN(len(p))
		if n%3 == 0 { // a chunk header or plane byte, where the checks are
			c := r.IntN(fixedChunks(len(src) / 2))
			at = fixedHeaderLen + c*fixedChunkLen + r.IntN(fixedChunkHead+3*fixedPlaneLen)
		}
		p[at] ^= byte(1 << r.IntN(8))
		decErr := decodeFixed(make([]byte, len(src)), p, nil)
		_, scanErr := scanFixed(p, len(src), &fixedPatch{split: len(src) / 2, inline: -1})
		if (decErr == nil) != (scanErr == nil) || scanErr != nil && !errors.Is(scanErr, ErrCorrupt) {
			t.Fatalf("byte %d flipped: decode %v, scan %v", at, decErr, scanErr)
		}
		if scanErr != nil {
			refused++
		}
	}
	if refused < 300 {
		t.Fatalf("only %d of 3000 changes refused", refused)
	}
	if _, err := scanFixed(good, len(src)+2, &fixedPatch{inline: -1}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("another decoded length: %v", err)
	}
}

// fusedCPU is the fused CPU decode of a pack: bytes with escapes left, and
// the patches.
func fusedCPU(t *testing.T, index Index, packed []byte, n int) ([]byte, []*fixedPatch) {
	t.Helper()
	dst := make([]byte, n)
	patches := make([]*fixedPatch, len(index.Chunks))
	if err := unpack(context.Background(), dst, index, index.Spans, 4, false, memOpen(packed), patches, nil, nil); err != nil {
		t.Fatal(err)
	}
	return dst, patches
}

func memOpen(packed []byte) openFunc {
	read := readerAt(bytes.NewReader(packed))
	return func() (readFunc, func(), error) { return read, func() {}, nil }
}

// applyEscapes runs a fill's hash pass over dst, every span at once.
func applyEscapes(dst []byte, index Index, patches []*fixedPatch) {
	finish := escapePass(dst, index.Chunks, patches)
	if finish == nil {
		return
	}
	var wg sync.WaitGroup
	for s := range index.Spans {
		wg.Go(func() {
			span := spanOf(dst, s)
			complete := finish(s)
			for at := 0; at < len(span); at += hashBlockForTest {
				complete(span[at:min(at+hashBlockForTest, len(span))])
			}
		})
	}
	wg.Wait()
}

func samePatches(a, b []*fixedPatch) bool {
	return slices.EqualFunc(a, b, func(x, y *fixedPatch) bool {
		if x == nil || y == nil {
			return x == y
		}
		return bytes.Equal(x.bases, y.bases) && bytes.Equal(x.escapes, y.escapes) &&
			slices.Equal(x.offsets, y.offsets) && slices.Equal(x.starts, y.starts) &&
			x.split == y.split && x.before == y.before && x.inline == y.inline
	})
}

// INF-MP7: each unit's recorded escape positions are exactly the values
// the fused decode left at their chunk's mark b+7, in value order, and each
// chunk's first escape is the index its header's bytes 4 to 7 hold.
func TestEscapePositionsAreTheMarkedValues(t *testing.T) {
	src, index, packed := fusedUnits(t)
	dst, patches := fusedCPU(t, index, packed, len(src))
	checked := 0
	for i, c := range index.Chunks {
		d := patches[i]
		if c.Encoding != EncodingExpFixed3V1 || d == nil {
			continue
		}
		p := packed[c.PackedOffset:][:c.Packed]
		// The value split across two spans took its escape at decode; every
		// other escaped value carries the mark, and no other value does.
		var marked []int
		for v := range int(c.Length / 2) {
			e := int(binary.LittleEndian.Uint16(dst[c.Offset+2*int64(v):])>>7) & 0xff
			if v != d.inline && e == int(d.bases[v/fixedChunkVals])+7 {
				marked = append(marked, v)
			}
		}
		var recorded []int
		for ch := range len(d.bases) {
			if got := binary.LittleEndian.Uint32(p[fixedHeaderLen+ch*fixedChunkLen+fixedEscapeAt:]); got != d.starts[ch] {
				t.Fatalf("unit at %d chunk %d: header escape index %d, recorded start %d", c.Offset, ch, got, d.starts[ch])
			}
			for k := d.starts[ch]; k < d.starts[ch+1]; k++ {
				recorded = append(recorded, ch*fixedChunkVals+int(d.offsets[k]))
			}
		}
		all := len(recorded)
		recorded = slices.DeleteFunc(recorded, func(v int) bool { return v == d.inline })
		if !slices.Equal(recorded, marked) || all != len(d.escapes) {
			t.Fatalf("unit at %d: %d recorded positions, %d marked values, %d escapes", c.Offset, len(recorded), len(marked), len(d.escapes))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no unit had escapes")
	}
}

// INF-MP7: apply writes a recorded position only where the value carries
// its chunk's mark b+7, consumes one escape per position, and leaves a
// marked value it has no position for.
func TestApplyWritesOnlyMarkedValues(t *testing.T) {
	const base = 100
	val := func(exp, low uint16) uint16 { return low&0x80<<8 | exp<<7 | low&0x7f }
	words := []uint16{val(base+7, 0x81), val(base+3, 0x05), val(base+7, 0x12), val(base+7, 0x7f)}
	view := values16(words...)
	d := &fixedPatch{
		bases:   []byte{base},
		escapes: []byte{90, 91, 92},
		offsets: []uint16{0, 1, 3}, // value 1 is not marked; value 2 is marked and not listed
		starts:  []uint32{0, 3},
		split:   len(words),
		inline:  -1,
	}
	if k := d.apply(view, 0, 0, len(words), 0); k != 3 {
		t.Fatalf("apply consumed %d escapes, want 3", k)
	}
	want := values16(val(90, 0x81), val(base+3, 0x05), val(base+7, 0x12), val(92, 0x7f))
	if !bytes.Equal(view, want) {
		t.Fatalf("view % x, want % x", view, want)
	}
}

// INF-MP7: once a fill's pass has gone over every span, it holds no
// unit's patch; the bytes are the object's.
func TestEscapePassFreesPatches(t *testing.T) {
	src, index, packed := fusedUnits(t)
	dst, patches := fusedCPU(t, index, packed, len(src))
	if !slices.ContainsFunc(patches, func(d *fixedPatch) bool { return d != nil && d.inline >= 0 }) {
		t.Fatal("no unit crosses a span with an escape")
	}
	applyEscapes(dst, index, patches)
	if i := slices.IndexFunc(patches, func(d *fixedPatch) bool { return d != nil }); i >= 0 {
		t.Fatalf("unit %d's patch is still held after the pass", i)
	}
	if !bytes.Equal(dst, src) {
		t.Fatal("the bytes after the pass differ from the object")
	}
}

// tamperings alter one unit's escape positions as a bug or a corrupted
// memory could, at escape k, one whose exponent is not its chunk's mark
// b+7: each must leave bytes the proof refuses, and none may make the pass
// read or write outside the unit. (An escaped value whose exponent is b+7
// reads right patched or not, so altering only its position changes no
// byte, and the proof rightly passes.)
var tamperings = map[string]func(d *fixedPatch, k int){
	"position moved by one":            func(d *fixedPatch, k int) { d.offsets[k] ^= 1 },
	"position moved to another escape": func(d *fixedPatch, k int) { d.offsets[k] = d.offsets[(k+1)%len(d.offsets)] },
	"positions swapped": func(d *fixedPatch, k int) {
		j := (k + len(d.offsets)/2) % len(d.offsets)
		d.offsets[k], d.offsets[j] = d.offsets[j], d.offsets[k]
	},
	"position dropped":  func(d *fixedPatch, k int) { d.offsets = slices.Delete(d.offsets, k, k+1) },
	"positions dropped": func(d *fixedPatch, _ int) { d.offsets = nil },
	"chunk start moved": func(d *fixedPatch, k int) { // escape k now counts in a neighbouring chunk
		c := sort.Search(len(d.starts), func(i int) bool { return d.starts[i] > uint32(k) }) - 1
		if c > 0 {
			d.starts[c] = uint32(k + 1)
		} else {
			d.starts[1] = uint32(k)
		}
	},
	"chunk starts cut":        func(d *fixedPatch, _ int) { d.starts = d.starts[:1] },
	"position past the chunk": func(d *fixedPatch, k int) { d.offsets[k] = 0xffff },
	"escape byte changed":     func(d *fixedPatch, k int) { d.escapes[k] ^= 1 },
}

// tamper applies one tampering to the first unit with escapes in two
// chunks, at its first escape whose exponent is not the mark, and reports
// whether it found one.
func tamper(patches []*fixedPatch, f func(*fixedPatch, int)) bool {
	for _, d := range patches {
		if d == nil || len(d.offsets) < 2 || len(d.starts) <= 2 || d.starts[1] == 0 {
			continue
		}
		c := 0
		for k := range d.offsets {
			for d.starts[c+1] <= uint32(k) {
				c++
			}
			if d.escapes[k] != d.bases[c]+7 && c*fixedChunkVals+int(d.offsets[k]) != d.inline {
				f(d, k)
				return true
			}
		}
	}
	return false
}

// INF-MP7: a wrong position list leaves bytes that differ from the object,
// so the spans differ from the record, and the pass stays inside the unit.
func TestTamperedEscapePositionsChangeTheBytes(t *testing.T) {
	src, index, packed := fusedUnits(t)
	for name, f := range tamperings {
		dst, patches := fusedCPU(t, index, packed, len(src))
		if !tamper(patches, f) {
			t.Fatal("no unit to tamper with")
		}
		applyEscapes(dst, index, patches)
		if slices.Equal(spanDigests(dst), index.Spans) {
			t.Fatalf("%s: the spans still match the record", name)
		}
	}
}

// INF-MP10: a decode through a device gives the bytes and patches of the
// fused CPU decode, for any slot count and buffer sets, a last batch that
// is not full included, and the hash pass then gives the object.
func TestDeviceFillMatchesTheCPU(t *testing.T) {
	src, index, packed := fusedUnits(t)
	want, wantPatches := fusedCPU(t, index, packed, len(src))
	fixed := encodings(index)[EncodingExpFixed3V1]
	for _, shape := range [][2]int{{4, 1}, {4, 2}, {5, 3}, {64, 2}} {
		dev := newFakeDevice(shape[0], shape[1])
		df := &deviceFill{dev: dev}
		dst := make([]byte, len(src))
		patches := make([]*fixedPatch, len(index.Chunks))
		if err := unpack(context.Background(), dst, index, index.Spans, 4, false, memOpen(packed), patches, df, nil); err != nil {
			t.Fatal(err)
		}
		var r FillReport
		df.report(&r)
		if r.DeviceUnits != fixed || r.CPUUnits != 0 || r.DeviceErr != nil || r.Batches != (fixed+shape[0]-1)/shape[0] {
			t.Fatalf("%v: report %+v for %d units", shape, r, fixed)
		}
		if dev.overlap.Load() {
			t.Fatalf("%v: two device runs overlapped", shape)
		}
		if !bytes.Equal(dst, want) || !samePatches(patches, wantPatches) {
			t.Fatalf("%v: the device decode differs from the fused CPU decode", shape)
		}
		applyEscapes(dst, index, patches)
		if !bytes.Equal(dst, src) {
			t.Fatalf("%v: the bytes after the hash pass differ from the object", shape)
		}
	}
}

// INF-MP10: a device error hands that batch and every later one to the
// CPU, which decodes them again from the pack; the bytes are the same.
func TestDeviceErrorFallsBackToTheCPU(t *testing.T) {
	src, index, packed := fusedUnits(t)
	want, wantPatches := fusedCPU(t, index, packed, len(src))
	fixed := encodings(index)[EncodingExpFixed3V1]
	for _, failAt := range []int64{1, 2} {
		dev := newFakeDevice(3, 2)
		dev.failAt = failAt
		df := &deviceFill{dev: dev}
		dst := make([]byte, len(src))
		patches := make([]*fixedPatch, len(index.Chunks))
		if err := unpack(context.Background(), dst, index, index.Spans, 4, false, memOpen(packed), patches, df, nil); err != nil {
			t.Fatal(err)
		}
		var r FillReport
		df.report(&r)
		if r.DeviceErr == nil || r.DeviceUnits != 3*int(failAt-1) || r.DeviceUnits+r.CPUUnits != fixed || dev.decodes.Load() != failAt {
			t.Fatalf("fail at run %d: report %+v, %d runs", failAt, r, dev.decodes.Load())
		}
		if !bytes.Equal(dst, want) || !samePatches(patches, wantPatches) {
			t.Fatalf("fail at run %d: the bytes differ from the fused CPU decode", failAt)
		}
	}
}

// INF-MP10: a device that already failed is not run again; the CPU decodes
// everything.
func TestFailedDeviceIsNotRunAgain(t *testing.T) {
	src, index, packed := fusedUnits(t)
	want, _ := fusedCPU(t, index, packed, len(src))
	dev := newFakeDevice(4, 2)
	dev.Distrust(errors.New("earlier fault"))
	df := &deviceFill{dev: dev}
	dst := make([]byte, len(src))
	patches := make([]*fixedPatch, len(index.Chunks))
	if err := unpack(context.Background(), dst, index, index.Spans, 4, false, memOpen(packed), patches, df, nil); err != nil {
		t.Fatal(err)
	}
	if dev.decodes.Load() != 0 || !bytes.Equal(dst, want) {
		t.Fatalf("%d runs on a failed device", dev.decodes.Load())
	}
}

// INF-MP10: a corrupt unit is refused on the device path as on the CPU
// path, before the device runs its batch.
func TestDeviceFillRefusesACorruptUnit(t *testing.T) {
	src, index, packed := fusedUnits(t)
	packed = bytes.Clone(packed)
	for _, c := range index.Chunks {
		if c.Encoding == EncodingExpFixed3V1 {
			packed[c.PackedOffset+fixedHeaderLen] = 250 // the first chunk's window base
			break
		}
	}
	dev := newFakeDevice(64, 2)
	patches := make([]*fixedPatch, len(index.Chunks))
	err := unpack(context.Background(), make([]byte, len(src)), index, index.Spans, 4, false, memOpen(packed), patches, &deviceFill{dev: dev}, nil)
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "window base") || dev.decodes.Load() != 0 {
		t.Fatalf("corrupt unit: %v after %d runs", err, dev.decodes.Load())
	}
	if err := unpack(context.Background(), make([]byte, len(src)), index, index.Spans, 4, false, memOpen(packed), nil, &deviceFill{dev: dev}, nil); err == nil {
		t.Fatal("a device decode without patches was accepted")
	}
}

// INF-MP10: the self-test passes a device that decodes as the kernel does
// and refuses one bit of difference, wrong buffer sizes and a device in use.
func TestCheckFixedDevice(t *testing.T) {
	ctx := context.Background()
	dev := newFakeDevice(5, 2)
	if err := CheckFixedDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	if dev.decodes.Load() != 2 || dev.busy.Load() {
		t.Fatalf("%d runs, busy %v", dev.decodes.Load(), dev.busy.Load())
	}
	bad := newFakeDevice(5, 2)
	bad.flipAt = 2
	if err := CheckFixedDevice(ctx, bad); err == nil || !strings.Contains(err.Error(), "set 1 slot 0") {
		t.Fatalf("a flipped bit: %v", err)
	}
	failing := newFakeDevice(5, 2)
	failing.failAt = 1
	if err := CheckFixedDevice(ctx, failing); err == nil {
		t.Fatal("a failing device passed")
	}
	short := newFakeDevice(5, 1)
	short.out[0] = short.out[0][:len(short.out[0])-1]
	if err := CheckFixedDevice(ctx, short); err == nil || short.decodes.Load() != 0 {
		t.Fatalf("a short output buffer: %v", err)
	}
	held := newFakeDevice(5, 1)
	held.TryAcquire()
	if err := CheckFixedDevice(ctx, held); err == nil || held.decodes.Load() != 0 {
		t.Fatalf("a device in use: %v", err)
	}
}
