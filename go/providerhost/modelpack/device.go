package modelpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// FixedSlot is the size of one unit's input slot in a FixedDevice batch:
// the most an exp-fixed3-v1 unit of ChunkBytes takes, its 64-byte header,
// 64 chunks of 5,696 bytes and one escape byte per eight values (INF-MP8).
const FixedSlot = fixedHeaderLen + (ChunkBytes/2/fixedChunkVals)*fixedChunkLen + ChunkBytes/2/fixedEscapeShare

// FixedDevice decodes exp-fixed3-v1 units beside the CPU in batches, such
// as an NPU (package npudecode). A batch is Slots() units in one of Sets()
// buffer sets: In(set) holds Slots() slots of FixedSlot bytes, a unit's
// payload at the start of its slot; Decode decodes the 64 chunks after each
// slot's 64-byte header into Out(set), ChunkBytes per slot, each value
// with exponent b + code, so an escaped value leaves with exponent b + 7 as
// the fused CPU decode leaves it. Bytes past a unit's values, in its slot
// or its output, are unspecified. A fill never trusts the output: every
// byte reaches the fill's span proof (INF-MP10).
type FixedDevice interface {
	Slots() int
	Sets() int
	In(set int) []byte
	Out(set int) []byte
	// Decode decodes every slot of set once, within the device's time
	// limit. After an error the device refuses every later Decode and
	// Err reports why.
	Decode(ctx context.Context, set int) error
	// TryAcquire claims the device for one fill, reporting false while
	// another fill holds it; Done gives it back. One fill at a time keeps
	// one device workload at a time.
	TryAcquire() bool
	Done()
	// Err is nil while the device may decode.
	Err() error
	// Distrust makes the device refuse every later Decode with err: its
	// output failed a proof the CPU decode of the same units passed.
	Distrust(err error)
}

// FillReport says how a fill decoded its exp-fixed3-v1 units.
type FillReport struct {
	// DeviceUnits were decoded on the device, in Batches device runs.
	DeviceUnits, Batches int
	// CPUUnits were decoded on the CPU: with no device, units larger than
	// a slot, and the units of a failed run and of every later batch.
	CPUUnits int
	// DeviceErr is the device error after which the CPU took over.
	DeviceErr error
	// Busy: another fill held the device, and this one used the CPU.
	Busy bool
	// Retried: the proof refused the bytes of a fill the device took part
	// in, and the fill ran again on the CPU. When that fill was proven the
	// device was distrusted.
	Retried bool
	// Decode is how long reading and decoding took, before the fill's hash
	// pass; DeviceTime is the part spent waiting on device runs.
	Decode, DeviceTime time.Duration
}

// deviceFill is one fill's use of a FixedDevice inside unpack.
type deviceFill struct {
	dev     FixedDevice
	units   []int // the units the device takes, in order
	cpu     int   // exp-fixed3-v1 units the CPU workers keep
	devDone atomic.Int64
	cpuDone atomic.Int64
	batches atomic.Int64
	devNS   atomic.Int64
	errMu   sync.Mutex
	err     error
}

// deviceUnit reports whether a FixedDevice can take unit c.
func deviceUnit(c *ChunkEntry) bool {
	return c.Encoding == EncodingExpFixed3V1 && c.Length <= ChunkBytes && c.Packed <= FixedSlot
}

// take marks the units the device decodes; unpack's CPU workers skip them.
func (f *deviceFill) take(units []ChunkEntry) []bool {
	taken := make([]bool, len(units))
	for i := range units {
		switch {
		case deviceUnit(&units[i]):
			taken[i] = true
			f.units = append(f.units, i)
		case units[i].Encoding == EncodingExpFixed3V1:
			f.cpu++
		}
	}
	return taken
}

// report copies what the fill did into r.
func (f *deviceFill) report(r *FillReport) {
	r.DeviceUnits = int(f.devDone.Load())
	r.CPUUnits = f.cpu + int(f.cpuDone.Load())
	r.Batches = int(f.batches.Load())
	r.DeviceTime = time.Duration(f.devNS.Load())
	f.errMu.Lock()
	r.DeviceErr = f.err
	f.errMu.Unlock()
}

// fixedBatch is one batch of units on the device.
type fixedBatch struct {
	set       int
	units     []int
	patch     []*fixedPatch
	inline    []int // the exponent byte of each unit's inline escape, -1 when none
	prep, fin sync.WaitGroup
}

// job is work for one of run's helpers, each with its own reader and
// buffer; read is nil when the helper could not open one.
type job func(read readFunc, buf *[]byte)

// run decodes f.units: helpers read each batch's units into a free buffer
// set and check them (scanFixed), the device decodes the set, and helpers
// copy each unit's bytes into dst and keep its patch for escapePass. Batch
// b+1 is read while batch b decodes, and batch b is copied while batch b+1
// decodes. A device error ends device use for the fill: that batch and
// every later one are read and decoded again on the CPU (decodeUnit).
func (f *deviceFill) run(ctx context.Context, dst []byte, units []ChunkEntry, open openFunc, helpers int, maxPacked int64, patches []*fixedPatch, fail func(error), failed *atomic.Bool) {
	if len(f.units) == 0 {
		return
	}
	slots, sets := f.dev.Slots(), f.dev.Sets()
	jobs := make(chan job, 2*slots)
	var hw sync.WaitGroup
	for range max(helpers, 1) {
		hw.Go(func() {
			read, closeRead, err := open()
			if err != nil {
				fail(err)
				for j := range jobs {
					j(nil, nil)
				}
				return
			}
			defer closeRead()
			var buf []byte
			for j := range jobs {
				j(read, &buf)
			}
		})
	}
	var bs []*fixedBatch
	for at := 0; at < len(f.units); at += slots {
		u := f.units[at:min(at+slots, len(f.units))]
		b := &fixedBatch{set: len(bs) % sets, units: u, patch: make([]*fixedPatch, len(u)), inline: make([]int, len(u))}
		bs = append(bs, b)
	}
	prepare := func(b *fixedBatch) {
		in := f.dev.In(b.set)
		for k, i := range b.units {
			b.prep.Add(1)
			jobs <- func(read readFunc, _ *[]byte) {
				defer b.prep.Done()
				if read == nil || failed.Load() {
					return
				}
				c := &units[i]
				slot := in[k*FixedSlot:][:c.Packed]
				p, err := read(c, slot)
				if err != nil {
					fail(err)
					return
				}
				if &p[0] != &slot[0] {
					copy(slot, p)
				}
				d := newFixedPatch(c)
				e, err := scanFixed(slot, int(c.Length), d)
				if err != nil {
					fail(fmt.Errorf("unit at %d: %w", c.Offset, err))
					return
				}
				b.patch[k], b.inline[k] = d, e
			}
		}
	}
	finish := func(b *fixedBatch, decoded bool) {
		out := f.dev.Out(b.set)
		for k, i := range b.units {
			b.fin.Add(1)
			jobs <- func(read readFunc, buf *[]byte) {
				defer b.fin.Done()
				if failed.Load() {
					return
				}
				c := &units[i]
				if !decoded {
					if read == nil {
						return
					}
					if err := decodeUnit(dst, c, read, buf, maxPacked, patches, i, nil); err != nil {
						fail(err)
						return
					}
					f.cpuDone.Add(1)
					return
				}
				slot := dst[c.Offset : c.Offset+c.Length]
				copy(slot, out[k*ChunkBytes:][:c.Length])
				d := b.patch[k]
				if e := b.inline[k]; e >= 0 {
					o := 2 * d.inline
					binary.LittleEndian.PutUint16(slot[o:], binary.LittleEndian.Uint16(slot[o:])&0x807f|uint16(e)<<7)
				}
				if len(d.escapes) > 0 {
					patches[i] = d
				}
				f.devDone.Add(1)
			}
		}
	}

	healthy := f.dev.Err() == nil
	if healthy {
		prepare(bs[0])
	}
	for n, b := range bs {
		b.prep.Wait()
		if sets > 1 && n+1 < len(bs) && healthy {
			prepare(bs[n+1])
		}
		if n >= sets {
			bs[n-sets].fin.Wait() // its output buffer is this batch's
		}
		if err := ctx.Err(); err != nil {
			fail(err)
		}
		if failed.Load() {
			break
		}
		decoded := false
		if healthy {
			start := time.Now()
			err := f.dev.Decode(ctx, b.set)
			f.devNS.Add(int64(time.Since(start)))
			if err != nil {
				healthy = false
				f.errMu.Lock()
				f.err = err
				f.errMu.Unlock()
				log.Printf("modelpack: the device failed a batch of %d exp-fixed3-v1 units (%v); the CPU decodes them and every later unit of this fill", len(b.units), err)
			} else {
				decoded = true
				f.batches.Add(1)
			}
		}
		if sets == 1 && n+1 < len(bs) && healthy {
			prepare(bs[n+1])
		}
		finish(b, decoded)
	}
	for _, b := range bs {
		b.prep.Wait()
		b.fin.Wait()
	}
	close(jobs)
	hw.Wait()
}

// chunkEscapes appends to offs, in value order, the index of each value
// below vals of a chunk whose code is 7, the values whose bit is set in all
// three planes. Value 512·blk + 64·i + j has its bit in bit i of byte
// 64·blk + j.
func chunkEscapes(chunk []byte, vals int, offs []uint16) []uint16 {
	p0 := chunk[fixedChunkHead:][:fixedPlaneLen]
	p1 := chunk[fixedChunkHead+fixedPlaneLen:][:fixedPlaneLen]
	p2 := chunk[fixedChunkHead+2*fixedPlaneLen:][:fixedPlaneLen]
	for blk := 0; blk < 8 && 512*blk < vals; blk++ {
		var a [8]uint64
		var all uint64
		for g := range 8 {
			at := 64*blk + 8*g
			a[g] = binary.LittleEndian.Uint64(p0[at:]) & binary.LittleEndian.Uint64(p1[at:]) & binary.LittleEndian.Uint64(p2[at:])
			all |= a[g]
		}
		if all == 0 {
			continue
		}
		for i := range 8 {
			for g := range 8 {
				for m := a[g] >> i & lanes8; m != 0; m &= m - 1 {
					if v := 512*blk + 64*i + 8*g + bits.TrailingZeros64(m)/8; v < vals {
						offs = append(offs, uint16(v))
					}
				}
			}
		}
	}
	return offs
}

// scanFixed checks payload p of a unit of l decoded bytes as decodeFixed
// does, without decoding it, and fills d as decodeFixed fills it, escape
// positions included, for a decoder that leaves every escaped value at
// exponent b + 7 (a FixedDevice). It returns the exponent byte of
// d.inline's escape, -1 when d.inline is not an escaped value.
func scanFixed(p []byte, l int, d *fixedPatch) (int, error) {
	n, body, err := fixedHeader(p, l)
	if err != nil {
		return -1, err
	}
	esc := p[body:]
	chunks := fixedChunks(n)
	d.bases = make([]byte, chunks)
	d.escapes = bytes.Clone(esc)
	d.offsets = make([]uint16, 0, len(esc))
	d.starts = make([]uint32, chunks+1)
	for c := range chunks {
		chunk := p[fixedHeaderLen+c*fixedChunkLen:][:fixedChunkLen]
		base, err := fixedChunkHeader(chunk, c, len(d.offsets))
		if err != nil {
			return -1, err
		}
		d.bases[c], d.starts[c] = base, uint32(len(d.offsets))
		d.offsets = chunkEscapes(chunk, min(fixedChunkVals, n-c*fixedChunkVals), d.offsets)
		if len(d.offsets) > len(esc) {
			return -1, corrupt("chunk %d: more escaped values than the %d escapes", c, len(esc))
		}
	}
	d.starts[chunks] = uint32(len(d.offsets))
	if len(d.offsets) != len(esc) {
		return -1, corrupt("%d escapes for %d escaped values", len(esc), len(d.offsets))
	}
	// below is the escape index of value v: the escapes before it.
	below := func(v int) int {
		c := v / fixedChunkVals
		in := d.offsets[d.starts[c]:d.starts[c+1]]
		return int(d.starts[c]) + sort.Search(len(in), func(i int) bool { return int(in[i]) >= v%fixedChunkVals })
	}
	d.before = len(esc)
	if d.split < n {
		d.before = below(d.split)
	}
	if d.inline >= 0 && d.inline < n {
		c := d.inline / fixedChunkVals
		if k := below(d.inline); k < int(d.starts[c+1]) && int(d.offsets[k]) == d.inline%fixedChunkVals {
			return int(esc[k]), nil
		}
	}
	return -1, nil
}

// fixedVectors are the known units CheckFixedDevice decodes: two whole
// units with escapes in every chunk and a short one whose last chunk is
// padded.
var fixedVectors = sync.OnceValues(func() ([][]byte, error) {
	var out [][]byte
	for _, v := range []struct {
		n    int
		seed uint64
		nan  int
	}{{ChunkBytes / 2, 0x0a11, 331}, {ChunkBytes / 2, 0x0b22, 97}, {100_003, 0x0c33, 1009}} {
		src := weights(v.n, v.seed, bf16)
		for i := 0; i < len(src); i += 2 * v.nan {
			binary.LittleEndian.PutUint16(src[i:], 0x7fc1)
		}
		p, err := EncodeExpFixed3(nil, src, "BF16")
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
})

// CheckFixedDevice is a device's self-test: it fills every slot of every
// buffer set with known exp-fixed3-v1 units, decodes each set on dev, and
// compares every unit's bytes with the CPU decode, bit for bit. A device
// that fails it, or whose buffers have other sizes, must not be given to a
// fill. It claims the device while it runs.
func CheckFixedDevice(ctx context.Context, dev FixedDevice) error {
	slots, sets := dev.Slots(), dev.Sets()
	if slots <= 0 || sets <= 0 {
		return fmt.Errorf("modelpack: a device of %d slots in %d sets", slots, sets)
	}
	for set := range sets {
		if len(dev.In(set)) != slots*FixedSlot || len(dev.Out(set)) != slots*ChunkBytes {
			return fmt.Errorf("modelpack: device set %d holds %d and %d bytes, want %d and %d",
				set, len(dev.In(set)), len(dev.Out(set)), slots*FixedSlot, slots*ChunkBytes)
		}
	}
	vectors, err := fixedVectors()
	if err != nil {
		return err
	}
	want := make([][]byte, len(vectors))
	for i, p := range vectors {
		n := int(binary.LittleEndian.Uint32(p[4:]))
		want[i] = make([]byte, 2*n)
		if err := decodeFixed(want[i], p, &fixedPatch{split: n, inline: -1}); err != nil {
			return err
		}
	}
	if !dev.TryAcquire() {
		return errors.New("modelpack: the device is busy")
	}
	defer dev.Done()
	for set := range sets {
		in, out := dev.In(set), dev.Out(set)
		for k := range slots {
			copy(in[k*FixedSlot:], vectors[(k+set)%len(vectors)])
		}
		if err := dev.Decode(ctx, set); err != nil {
			return fmt.Errorf("modelpack: device self-test: %w", err)
		}
		for k := range slots {
			w := want[(k+set)%len(want)]
			got := out[k*ChunkBytes:][:len(w)]
			if !bytes.Equal(got, w) {
				at := 0
				for got[at] == w[at] {
					at++
				}
				return fmt.Errorf("modelpack: device self-test: set %d slot %d differs from the CPU decode at byte %d", set, k, at)
			}
		}
	}
	return nil
}
