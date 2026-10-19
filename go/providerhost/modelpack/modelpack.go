// Package modelpack is the encoding of OA's model packs: how an object's
// bytes are cut into encoding units, which units the in-house exponent codec
// stores, and how a provider host decodes them in parallel straight into
// the memory it lends to engines (VISION 2026-10-01; inference CONTRACT.md
// INF-MP1..INF-MP5; research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md).
//
// Pack cuts an object into encoding units and records them in an Index with
// the proof of the object: the SHA-256 of each modelfiles.ChunkSize span of
// its decoded bytes, the digests a memory section records when it is filled
// (INF-MS2). A unit of a BF16 or F16 tensor is stored as exp-huffman-v1 when
// that saves at least 3 percent and the machine decodes that dtype faster
// than its disk reads; every other unit, quantised tensors included, is
// stored as is. Unpack reads each unit with one positioned read and decodes
// it into its place in a caller's buffer, the writable view of a section, on
// several goroutines, and checks each span once the units covering it are
// decoded. Nothing is written to a file.
//
// The package sits beside providerhost/modelfiles, which owns the section a
// pack is decoded into and the span size; the zip container a pack lives in
// is outside it.
package modelpack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// Encoding names how one unit's bytes are stored in a pack.
type Encoding string

const (
	// EncodingStored units hold their bytes as they are.
	EncodingStored Encoding = "stored"
	// EncodingExpHuffmanV1 units hold 16-bit floats with their exponent byte
	// Huffman-coded (INF-MP3).
	EncodingExpHuffmanV1 Encoding = "exp-huffman-v1"
	// EncodingExpFixed3V1 units hold BF16 values with their exponent in 3
	// fixed bits within a window of seven, and escapes (INF-MP8).
	EncodingExpFixed3V1 Encoding = "exp-fixed3-v1"
	// EncodingExpANSV1 units hold 16-bit floats in two byte planes coded
	// with OA's rANS coder (INF-MP11).
	EncodingExpANSV1 Encoding = "exp-ans-v1"
)

// decoderFor is the decoder of an encoded unit, nil for any other encoding.
func decoderFor(e Encoding) func(dst, payload []byte) error {
	switch e {
	case EncodingExpHuffmanV1:
		return DecodeChunk
	case EncodingExpFixed3V1:
		return DecodeExpFixed3
	case EncodingExpANSV1:
		return func(dst, p []byte) error { return DecodeExpANS(dst, p, nil) }
	}
	return nil
}

// ChunkBytes is the decoded size of an exp-huffman-v1 encoding unit: 262,144
// values of 16 bits. A tensor's last unit may be shorter.
const ChunkBytes = 512 << 10

// ChunkEntry is one encoding unit of a pack: the decoded bytes
// [Offset, Offset+Length) of the object, stored as Packed bytes at
// PackedOffset of the object's packed bytes, in Encoding.
type ChunkEntry struct {
	Offset       int64    `json:"offset"`
	Length       int64    `json:"length"`
	PackedOffset int64    `json:"packed_offset"`
	Packed       int64    `json:"packed"`
	Encoding     Encoding `json:"encoding"`
	// Transform, for a delta unit (INF-MP12), is int-diff-v1, and
	// BaseOffset the offset of the unit's Length bytes in the index's base.
	Transform  string `json:"transform,omitempty"`
	BaseOffset int64  `json:"base_offset,omitempty"`
}

// Index is a packed object's index: its decoded Length, its encoding units
// in order, and Spans, the lower-case hex SHA-256 of each
// modelfiles.ChunkSize span of its decoded bytes, in the form
// modelfiles.Section.ChunkDigests reports. Spans read back from a pack are
// a claim; they prove bytes only as INF-MP1 says, through Unpack's spans
// argument.
type Index struct {
	// Digest is the object's "sha256:" digest, which binds the index to it.
	Digest string       `json:"digest"`
	Length int64        `json:"length"`
	Chunks []ChunkEntry `json:"chunks"`
	Spans  []string     `json:"spans"`
	// Base, when the object holds delta units, names the base object they
	// are coded against (INF-MP12).
	Base *BaseRef `json:"base,omitempty"`
}

// Tensor is the range [Offset, Offset+Length) of an object's bytes that
// holds one tensor, and its DType as safetensors or GGUF names it; Name and
// Shape, when known, go into an archive's tensor table (INF-MP6).
type Tensor struct {
	Offset int64
	Length int64
	DType  string
	Name   string
	Shape  []int64
}

// Rates are a machine's measured rates in bytes per second, aggregate over
// the goroutines a fill uses, in decoded bytes: BF16 and F16 for
// exp-huffman-v1, BF16Fixed3 for exp-fixed3-v1, and Disk, the read rate of
// the volume the pack is on. A rate of zero offers no encoding.
type Rates struct {
	BF16       float64
	F16        float64
	BF16Fixed3 float64
	Disk       float64
}

// unitEncoder is one encoding Pack may try for a unit of a dtype, with the
// machine's decode rate for it.
type unitEncoder struct {
	enc    Encoding
	rate   float64
	encode func(dst, src []byte, dtype string) ([]byte, error)
}

// encoders lists the encodings that decode a unit of dtype faster than the
// disk reads; with no measured disk rate it lists none.
func (r Rates) encoders(dtype string) []unitEncoder {
	if r.Disk <= 0 {
		return nil
	}
	var out []unitEncoder
	switch dtype {
	case "BF16":
		if r.BF16 > r.Disk {
			out = append(out, unitEncoder{EncodingExpHuffmanV1, r.BF16, encodeUnit})
		}
		if r.BF16Fixed3 > r.Disk {
			out = append(out, unitEncoder{EncodingExpFixed3V1, r.BF16Fixed3, encodeFixed})
		}
	case "F16":
		if r.F16 > r.Disk {
			out = append(out, unitEncoder{EncodingExpHuffmanV1, r.F16, encodeUnit})
		}
	}
	return out
}

// encodes reports whether units of dtype are worth encoding on this machine.
func (r Rates) encodes(dtype string) bool { return len(r.encoders(dtype)) > 0 }

// loadTime is the time one unit of length decoded bytes, stored in size
// bytes, takes to load: the read of its stored bytes or its decode at rate,
// whichever is longer, as reads and decodes overlap across units. A stored
// unit has no decode (rate 0).
func loadTime(size, length int64, rate, disk float64) float64 {
	t := float64(size) / disk
	if rate > 0 {
		t = max(t, float64(length)/rate)
	}
	return t
}

var (
	// ErrDType: the bytes are not 16-bit floats the codec takes.
	ErrDType = errors.New("modelpack: not BF16 or F16 values")
	// ErrCorrupt: a pack index or a unit's stored bytes cannot be decoded
	// as described: truncated, malformed or inconsistent.
	ErrCorrupt = errors.New("modelpack: corrupt pack")
)

// encodeUnit and encodeFixed are the encoders Pack uses; a test replaces
// one with a broken encoder to show Pack's decode check refuses its output.
var (
	encodeUnit  = EncodeChunk
	encodeFixed = EncodeExpFixed3
)

// segment is a run of the object's bytes packed one way: encoded per
// ChunkBytes as dtype, or stored when dtype is empty. base is the offset of
// the segment's bytes in a delta's base, -1 when it has none.
type segment struct {
	off, n int64
	dtype  string
	base   int64
}

// Pack writes the packed form of an object's bytes src to w and returns its
// index. tensors name the object's tensors, in order and without overlap;
// bytes outside them are stored. A BF16 or F16 tensor is cut into units of
// ChunkBytes from its start. Each unit is tried in every encoding rates say
// the machine decodes faster than the disk reads (exp-huffman-v1 for BF16
// and F16, exp-fixed3-v1 for BF16); an encoding counts only where it saves
// at least 3 percent of the unit. The unit takes the encoding whose load is
// shortest by loadTime, stored included, the smaller one on a tie: the
// fixed code where exp-huffman-v1 would wait on its decode, exp-huffman-v1
// where the decode keeps up with the disk (INF-MP2). An encoded unit is
// decoded before it is written, and one that decodes to other bytes refuses
// the pack (ErrCorrupt). Stored runs are cut into units of at most
// modelfiles.ChunkSize. A 16-bit tensor of an odd byte count is ErrDType.
// Index.Spans is OA's record of the object, made as Pack hashes src.
func Pack(w io.Writer, src []byte, tensors []Tensor, rates Rates) (Index, error) {
	return PackDelta(w, src, tensors, rates, nil)
}

// PackDelta packs as Pack does and, with base, also tries each unit of a
// BF16 tensor that base holds by the same name, dtype and length as a delta
// unit against the same bytes of base (INF-MP12): int-diff-v1 words coded
// exp-ans-v1 under the base's exponents. A delta unit replaces the unit's
// other choice where it is smaller and saves 3 percent of the unit, since a
// delta is chosen to save disk and download, and is decoded against the base
// before it is written. The index then names the base. Keep the base bytes
// immutable from ProveBase through this call's return, as ProvenBase
// documents; this call may read them while it packs.
func PackDelta(w io.Writer, src []byte, tensors []Tensor, rates Rates, base *DeltaBase) (Index, error) {
	baseOf := func(Tensor) int64 { return -1 }
	if base != nil {
		byName := map[string]Tensor{}
		for _, t := range base.Tensors {
			byName[t.Name] = t
		}
		blen := int64(len(base.Base.b))
		baseOf = func(t Tensor) int64 {
			bt, ok := byName[t.Name]
			if !ok || t.DType != "BF16" || bt.DType != "BF16" || bt.Length != t.Length || bt.Offset < 0 || bt.Offset > blen-bt.Length {
				return -1
			}
			return bt.Offset
		}
	}
	segs, err := plan(int64(len(src)), tensors, rates, baseOf)
	if err != nil {
		return Index{}, err
	}
	var units []ChunkEntry
	var dtypes []string
	var bases []int64
	for _, s := range segs {
		step := int64(modelfiles.ChunkSize)
		if s.dtype != "" {
			step = ChunkBytes
		}
		for off := s.off; off < s.off+s.n; off += step {
			units = append(units, ChunkEntry{Offset: off, Length: min(step, s.off+s.n-off), Encoding: EncodingStored})
			dtypes = append(dtypes, s.dtype)
			bo := int64(-1)
			if s.base >= 0 {
				bo = s.base + off - s.off
			}
			bases = append(bases, bo)
		}
	}
	index := Index{Length: int64(len(src)), Chunks: units}
	spans := make(chan []string, 1)
	whole := make(chan string, 1)
	go func() { spans <- spanDigests(src) }()
	go func() {
		sum := sha256.Sum256(src)
		whole <- "sha256:" + hex.EncodeToString(sum[:])
	}()

	workers := runtime.GOMAXPROCS(0)
	batch := 4 * workers
	payloads := make([][]byte, batch)
	var packed int64
	for first := 0; first < len(units); first += batch {
		last := min(first+batch, len(units))
		var next atomic.Int64
		next.Store(int64(first))
		var wg sync.WaitGroup
		var failed atomic.Pointer[error]
		for range min(workers, last-first) {
			wg.Go(func() {
				var check []byte
				refuse := func(u *ChunkEntry, err error) {
					err = fmt.Errorf("%w: unit at %d does not decode to its bytes (%v)", ErrCorrupt, u.Offset, err)
					failed.CompareAndSwap(nil, &err)
				}
				for i := int(next.Add(1) - 1); i < last; i = int(next.Add(1) - 1) {
					u := &units[i]
					b := src[u.Offset : u.Offset+u.Length]
					payloads[i-first] = b
					if dtypes[i] == "" {
						continue
					}
					if cap(check) < len(b) {
						check = make([]byte, ChunkBytes)
					}
					best, bestTime := EncodingStored, loadTime(u.Length, u.Length, 0, rates.Disk)
					var payload []byte
					for _, e := range rates.encoders(dtypes[i]) {
						// The fixed code refuses a unit with too many escapes;
						// dtype and length were checked for the others.
						p, err := e.encode(nil, b, dtypes[i])
						if err != nil || len(p) > len(b)-len(b)*3/100 {
							continue
						}
						t := loadTime(int64(len(p)), u.Length, e.rate, rates.Disk)
						if t < bestTime || t == bestTime && payload != nil && len(p) < len(payload) {
							best, bestTime, payload = e.enc, t, p
						}
					}
					if bo := bases[i]; bo >= 0 {
						bb := base.Base.b[bo : bo+u.Length]
						p, err := encodeDelta(b, bb)
						size := len(b)
						if payload != nil {
							size = len(payload)
						}
						if err == nil && len(p) <= len(b)-len(b)*3/100 && len(p) < size {
							// A delta unit is decoded against its base before it
							// is written, as every encoded unit is.
							if err := DecodeDelta(check[:len(b)], p, bb); err != nil || !bytes.Equal(check[:len(b)], b) {
								refuse(u, err)
								return
							}
							u.Encoding, u.Transform, u.BaseOffset, payloads[i-first] = EncodingExpANSV1, TransformIntDiffV1, bo, p
							continue
						}
					}
					if payload == nil {
						continue
					}
					// Each encoded unit is decoded before it is written, so a
					// pack never holds a unit that gives other bytes.
					if err := decoderFor(best)(check[:len(b)], payload); err != nil || !bytes.Equal(check[:len(b)], b) {
						refuse(u, err)
						return
					}
					u.Encoding, payloads[i-first] = best, payload
				}
			})
		}
		wg.Wait()
		if err := failed.Load(); err != nil {
			<-spans
			<-whole
			return Index{}, *err
		}
		for i := first; i < last; i++ {
			units[i].PackedOffset, units[i].Packed = packed, int64(len(payloads[i-first]))
			if _, err := w.Write(payloads[i-first]); err != nil {
				<-spans
				<-whole
				return Index{}, err
			}
			packed += units[i].Packed
			payloads[i-first] = nil
		}
	}
	for _, u := range units {
		if u.Transform != "" {
			index.Base = &BaseRef{Digest: base.Base.digest, Length: int64(len(base.Base.b))}
			break
		}
	}
	index.Spans, index.Digest = <-spans, <-whole
	return index, nil
}

// plan turns the object's tensors into segments that cover it in order,
// joining neighbouring stored bytes into one run. baseOf gives a tensor's
// offset in a delta's base, -1 for none; a tensor with a base is cut into
// units even when rates encode nothing.
func plan(size int64, tensors []Tensor, rates Rates, baseOf func(Tensor) int64) ([]segment, error) {
	var segs []segment
	add := func(s segment) {
		if s.n == 0 {
			return
		}
		if last := len(segs) - 1; last >= 0 && s.dtype == "" && segs[last].dtype == "" {
			segs[last].n += s.n
			return
		}
		segs = append(segs, s)
	}
	at := int64(0)
	for i, t := range tensors {
		if t.Offset < at || t.Length < 0 || t.Length > size-t.Offset {
			return nil, fmt.Errorf("modelpack: tensor %d at [%d, +%d) overlaps another or lies outside the %d-byte object", i, t.Offset, t.Length, size)
		}
		_, sixteen := dtypeByte(t.DType)
		if sixteen && t.Length%2 != 0 {
			return nil, fmt.Errorf("%w: %s tensor %d of %d bytes", ErrDType, t.DType, i, t.Length)
		}
		add(segment{off: at, n: t.Offset - at, base: -1})
		bo := int64(-1)
		if sixteen {
			bo = baseOf(t)
		}
		if sixteen && (rates.encodes(t.DType) || bo >= 0) {
			add(segment{off: t.Offset, n: t.Length, dtype: t.DType, base: bo})
		} else {
			add(segment{off: t.Offset, n: t.Length, base: -1})
		}
		at = t.Offset + t.Length
	}
	add(segment{off: at, n: size - at})
	return segs, nil
}

// spanDigests hashes each modelfiles.ChunkSize span of b on several
// goroutines.
func spanDigests(b []byte) []string {
	n := (len(b) + modelfiles.ChunkSize - 1) / modelfiles.ChunkSize
	out := make([]string, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), n) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				sum := sha256.Sum256(spanOf(b, i))
				out[i] = hex.EncodeToString(sum[:])
			}
		})
	}
	wg.Wait()
	return out
}

func spanOf(b []byte, i int) []byte {
	return b[i*modelfiles.ChunkSize : min((i+1)*modelfiles.ChunkSize, len(b))]
}

// Unpack decodes an object's packed bytes, read from src at the index's
// packed offsets, into dst, whose length must be the object's decoded
// length. It reads each unit with one positioned read and decodes it into
// its place on up to workers goroutines (GOMAXPROCS when workers is not
// positive). Once every unit covering a modelfiles.ChunkSize span is
// decoded, it checks the span's SHA-256 against spans.
//
// spans are the proof, and the caller takes them from OA's own record of
// the object (INF-MP1): the spans OA recorded when it hashed the object, or
// those of an index bound to the object's whole SHA-256 that OA verified
// once. The index's own Spans, read from a pack, prove nothing; a first
// fill from such a pack passes them and then hashes the whole object
// against its digest before the bytes count as proven.
//
// A malformed index, spans of the wrong count or form, or a short read is
// ErrCorrupt, a span of other bytes modelfiles.ErrDigest, and a cancelled
// context its error; dst then holds partial output.
func Unpack(ctx context.Context, dst []byte, src io.ReaderAt, index Index, spans []string, workers int) error {
	return UnpackWithBase(ctx, dst, src, index, spans, ProvenBase{}, workers)
}

// UnpackWithBase unpacks as Unpack does an object that may hold delta units
// (INF-MP12): base is its proven base, checked against the index's before
// any unit is read (ErrBase when missing, modelfiles.ErrDigest when another
// object); an object with no base ignores it. Keep the bytes borrowed by base
// immutable and alive until this call returns; its workers may read them
// concurrently.
func UnpackWithBase(ctx context.Context, dst []byte, src io.ReaderAt, index Index, spans []string, base ProvenBase, workers int) error {
	b, err := baseFor(index, base)
	if err != nil {
		return err
	}
	shared := readerAt(src)
	return unpack(ctx, dst, index, spans, workers, true, func() (readFunc, func(), error) {
		return shared, func() {}, nil
	}, nil, nil, b)
}

// readFunc returns unit c's packed bytes, in buf (len(buf) == c.Packed) or
// elsewhere.
type readFunc func(c *ChunkEntry, buf []byte) ([]byte, error)

// openFunc gives one worker its own readFunc and the function that closes it.
type openFunc func() (readFunc, func(), error)

// readerAt reads units from src at their packed offsets.
func readerAt(src io.ReaderAt) readFunc {
	return func(c *ChunkEntry, buf []byte) ([]byte, error) {
		n, err := src.ReadAt(buf, c.PackedOffset)
		if n < len(buf) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("%w: unit at %d: %w", ErrCorrupt, c.Offset, err)
		}
		return buf, nil
	}
}

// unpack decodes as Unpack does, checking each span against proof when
// verify is set. patches, when not nil, has one slot per unit: each
// exp-fixed3-v1 unit is then decoded with its escapes left for escapePass,
// and its slot holds its fixedPatch when it has escapes; verify is then
// unset, since the bytes are whole only after escapePass. dev, when not
// nil, needs patches and decodes the exp-fixed3-v1 units it takes in
// batches beside the CPU workers (deviceFill). base holds the index's
// base object for its delta units, already checked by baseFor.
func unpack(ctx context.Context, dst []byte, index Index, proof []string, workers int, verify bool, open openFunc, patches []*fixedPatch, dev *deviceFill, base []byte) error {
	if index.Base != nil && int64(len(base)) != index.Base.Length {
		return fmt.Errorf("%w: base %s", ErrBase, index.Base.Digest)
	}
	if index.Length != int64(len(dst)) {
		return fmt.Errorf("%w: object of %d bytes, the buffer holds %d", ErrCorrupt, index.Length, len(dst))
	}
	maxPacked, err := checkIndex(index)
	if err != nil {
		return err
	}
	spans, err := checkSpans(index.Length, proof)
	if err != nil {
		return err
	}
	units := index.Chunks
	if patches != nil && (verify || len(patches) != len(units)) {
		return errors.New("modelpack: a fused decode has one patch slot per unit and leaves the span check to the fill")
	}
	if dev != nil && patches == nil {
		return errors.New("modelpack: a device decode leaves escapes to the fill's hash pass")
	}
	// pending[s] counts the units of span s not yet decoded; the worker
	// that decodes the last one checks the span.
	pending := make([]atomic.Int32, len(spans))
	for i := range units {
		first, last := spanRange(&units[i])
		for s := first; s <= last; s++ {
			pending[s].Add(1)
		}
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(units))
	var (
		next   atomic.Int64
		failed atomic.Bool
		once   sync.Once
		first  error
		wg     sync.WaitGroup
	)
	fail := func(err error) {
		once.Do(func() { first = err })
		failed.Store(true)
	}
	var taken []bool
	if dev != nil {
		taken = dev.take(units)
		wg.Go(func() { dev.run(ctx, dst, units, open, workers, maxPacked, patches, fail, &failed) })
	}
	for range workers {
		wg.Go(func() {
			read, closeRead, err := open()
			if err != nil {
				fail(err)
				return
			}
			defer closeRead()
			var buf []byte
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= len(units) {
					return
				}
				if taken != nil && taken[i] {
					continue
				}
				if err := ctx.Err(); err != nil {
					fail(err)
					return
				}
				c := &units[i]
				if err := decodeUnit(dst, c, read, &buf, maxPacked, patches, i, base); err != nil {
					fail(err)
					return
				}
				lo, hi := spanRange(c)
				for s := lo; s <= hi; s++ {
					if pending[s].Add(-1) != 0 || !verify {
						continue
					}
					if sha256.Sum256(spanOf(dst, s)) != spans[s] {
						fail(fmt.Errorf("%w: span %d at byte %d", modelfiles.ErrDigest, s, s*modelfiles.ChunkSize))
						return
					}
				}
			}
		})
	}
	wg.Wait()
	return first
}

// decodeUnit reads unit i, c, through read and decodes it into its place in
// dst on the CPU, using *buf (grown to maxPacked) for encoded bytes. With
// patches an exp-fixed3-v1 unit leaves its escapes for escapePass, and
// patches[i] holds them. A delta unit decodes against its bytes of base
// (INF-MP12).
func decodeUnit(dst []byte, c *ChunkEntry, read readFunc, buf *[]byte, maxPacked int64, patches []*fixedPatch, i int, base []byte) error {
	slot := dst[c.Offset : c.Offset+c.Length]
	if c.Encoding == EncodingStored {
		b, err := read(c, slot)
		if err != nil {
			return err
		}
		if &b[0] != &slot[0] {
			copy(slot, b)
		}
		return nil
	}
	if *buf == nil {
		*buf = make([]byte, maxPacked)
	}
	b, err := read(c, (*buf)[:c.Packed])
	if err != nil {
		return err
	}
	if c.Transform != "" {
		if err := DecodeDelta(slot, b, base[c.BaseOffset:c.BaseOffset+c.Length]); err != nil {
			return fmt.Errorf("delta unit at %d: %w", c.Offset, err)
		}
		return nil
	}
	if patches != nil && c.Encoding == EncodingExpFixed3V1 {
		d := newFixedPatch(c)
		if err := decodeFixed(slot, b, d); err != nil {
			return fmt.Errorf("unit at %d: %w", c.Offset, err)
		}
		if len(d.escapes) > 0 {
			patches[i] = d
		}
		return nil
	}
	if err := decoderFor(c.Encoding)(slot, b); err != nil {
		return fmt.Errorf("unit at %d: %w", c.Offset, err)
	}
	return nil
}

// spanRange gives the first and last span unit c's bytes fall in.
func spanRange(c *ChunkEntry) (int, int) {
	return int(c.Offset / modelfiles.ChunkSize), int((c.Offset + c.Length - 1) / modelfiles.ChunkSize)
}

// maxPackedUnit bounds the stored bytes of an exp-huffman-v1 unit of n
// decoded bytes: its header and both planes coded with their tables, each
// plane at most the plane's raw size.
func maxPackedUnit(n int64) int64 { return headerLen + 2*codedPrefix + n }

// checkIndex accepts an index whose units tile its length in order, each
// with a known encoding, packed and decoded lengths equal when
// EncodingStored, at most 2 MiB decoded and at most maxPackedUnit stored
// when EncodingExpHuffmanV1, and an even length of at most 2 MiB stored in
// at most maxPackedFixed bytes when EncodingExpFixed3V1, and an even length
// of at most 2 MiB stored in at most its length plus the 16-byte header
// when EncodingExpANSV1; it returns the
// largest encoded unit.
func checkIndex(index Index) (int64, error) {
	var at, maxPacked int64
	deltas := 0
	for i, c := range index.Chunks {
		switch {
		case c.Offset != at || c.Length <= 0 || c.Length > index.Length-at:
			return 0, fmt.Errorf("%w: unit %d covers [%d, +%d), expected to start at %d within %d bytes",
				ErrCorrupt, i, c.Offset, c.Length, at, index.Length)
		case c.Packed <= 0 || c.PackedOffset < 0:
			return 0, fmt.Errorf("%w: unit %d has %d packed bytes at %d", ErrCorrupt, i, c.Packed, c.PackedOffset)
		case c.Encoding == EncodingStored && c.Packed != c.Length:
			return 0, fmt.Errorf("%w: stored unit %d of two sizes, %d and %d", ErrCorrupt, i, c.Packed, c.Length)
		case c.Encoding == EncodingExpHuffmanV1:
			if c.Length > 2*maxChunkValues || c.Packed > maxPackedUnit(c.Length) {
				return 0, fmt.Errorf("%w: encoded unit %d of %d bytes for %d", ErrCorrupt, i, c.Packed, c.Length)
			}
			maxPacked = max(maxPacked, c.Packed)
		case c.Encoding == EncodingExpANSV1:
			if c.Length > 2*maxChunkValues || c.Length%2 != 0 || c.Packed > ansHeaderLen+c.Length {
				return 0, fmt.Errorf("%w: ans unit %d of %d bytes for %d", ErrCorrupt, i, c.Packed, c.Length)
			}
			maxPacked = max(maxPacked, c.Packed)
		case c.Encoding == EncodingExpFixed3V1:
			if c.Length > 2*maxChunkValues || c.Length%2 != 0 || c.Packed > maxPackedFixed(c.Length) {
				return 0, fmt.Errorf("%w: fixed-code unit %d of %d bytes for %d", ErrCorrupt, i, c.Packed, c.Length)
			}
			maxPacked = max(maxPacked, c.Packed)
		case c.Encoding != EncodingStored:
			return 0, fmt.Errorf("%w: unit %d encoding %q", ErrCorrupt, i, c.Encoding)
		}
		if err := checkDeltaUnit(index, i, &c); err != nil {
			return 0, err
		}
		deltas += btoi(c.Transform != "")
		at += c.Length
	}
	if at != index.Length {
		return 0, fmt.Errorf("%w: units cover %d of %d bytes", ErrCorrupt, at, index.Length)
	}
	if index.Base != nil && deltas == 0 {
		return 0, fmt.Errorf("%w: the index names a base and holds no delta unit", ErrCorrupt)
	}
	return maxPacked, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkDeltaUnit accepts unit i, c, of index when it is no delta unit (no
// transform, no base offset) or a delta unit by INF-MP12: transform
// int-diff-v1, encoding exp-ans-v1, an even length, and a base offset (a
// tensor may start on any byte) whose Length bytes lie inside the base the index names, a well-formed
// digest of a positive length.
func checkDeltaUnit(index Index, i int, c *ChunkEntry) error {
	if c.Transform == "" {
		if c.BaseOffset != 0 {
			return fmt.Errorf("%w: unit %d has a base offset and no transform", ErrCorrupt, i)
		}
		return nil
	}
	switch {
	case c.Transform != TransformIntDiffV1:
		return fmt.Errorf("%w: unit %d transform %q", ErrCorrupt, i, c.Transform)
	case c.Encoding != EncodingExpANSV1:
		return fmt.Errorf("%w: delta unit %d coded %q, want %s", ErrCorrupt, i, c.Encoding, EncodingExpANSV1)
	case index.Base == nil:
		return fmt.Errorf("%w: delta unit %d in an index that names no base", ErrCorrupt, i)
	case modelfiles.CheckDigest(index.Base.Digest) != nil || index.Base.Length <= 0:
		return fmt.Errorf("%w: base %q of %d bytes", ErrCorrupt, index.Base.Digest, index.Base.Length)
	case c.Length%2 != 0 || c.BaseOffset < 0 || c.BaseOffset > index.Base.Length-c.Length:
		return fmt.Errorf("%w: delta unit %d of %d bytes at base offset %d in a base of %d", ErrCorrupt, i, c.Length, c.BaseOffset, index.Base.Length)
	}
	return nil
}

// checkSpans accepts one lower-case hex SHA-256 per modelfiles.ChunkSize
// span of an object of length bytes.
func checkSpans(length int64, proof []string) ([][32]byte, error) {
	want := (length + modelfiles.ChunkSize - 1) / modelfiles.ChunkSize
	if int64(len(proof)) != want {
		return nil, fmt.Errorf("%w: %d span digests for %d spans", ErrCorrupt, len(proof), want)
	}
	spans := make([][32]byte, len(proof))
	for i, d := range proof {
		if len(d) != 64 || strings.ToLower(d) != d {
			return nil, fmt.Errorf("%w: span %d digest %q", ErrCorrupt, i, d)
		}
		if _, err := hex.Decode(spans[i][:], []byte(d)); err != nil {
			return nil, fmt.Errorf("%w: span %d digest %q", ErrCorrupt, i, d)
		}
	}
	return spans, nil
}
