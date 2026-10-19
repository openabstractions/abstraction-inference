package modelpack

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// The exp-fixed3-v1 unit (inference CONTRACT.md INF-MP8): BF16 values with
// each exponent coded in 3 fixed bits as an offset into a window of seven
// exponents: ZipServ's TBE code (https://arxiv.org/html/2603.17435), as measured in
// research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md §12.2, whose
// chunk layout it keeps so the NPU decoder measured there reads the same
// bytes.
//
// A unit is a 64-byte header, then one 5,696-byte chunk per 4,096 values
// (the last chunk padded), then the escapes. A chunk is a 64-byte header
// whose first byte is the window base b and whose bytes 4 to 7 hold, as a
// little-endian 32-bit number, how many escapes the unit's earlier chunks
// use, so a decoder can start at any chunk; three 512-byte bit planes; and
// 4,096 bytes of sign and mantissa (sign << 7 | mantissa). Value
// v = 512·blk + 64·i + j of a chunk keeps the three bits of its code in bit
// i of byte 64·blk + j of each plane. The exponent is b + code for codes 0
// to 6; code 7 marks an escape, whose exponent byte is the next byte of the
// escape list, in value order.
const (
	fixedHeaderLen = 64
	fixedChunkVals = 4096
	fixedPlaneLen  = fixedChunkVals / 8
	fixedChunkHead = 64
	fixedChunkLen  = fixedChunkHead + 3*fixedPlaneLen + fixedChunkVals // 5,696
	// fixedEscapeAt is where a chunk header holds its first escape's index.
	fixedEscapeAt = 4
	fixedVersion  = 1
	// fixedMaxBase keeps b + 7 within a byte, so eight exponents add in one
	// 64-bit word without a carry between them.
	fixedMaxBase = 248
	// fixedEscapeShare: a unit whose escapes exceed one value in eight is
	// refused; Pack then stores it as exp-huffman-v1 or as it is.
	fixedEscapeShare = 8

	lanes8 = 0x0101010101010101
)

// errEscapes: more than one value in eight lies outside the unit's window.
var errEscapes = errors.New("modelpack: too many values outside the exponent window")

// fixedChunks is the number of chunks of a unit of n values.
func fixedChunks(n int) int { return (n + fixedChunkVals - 1) / fixedChunkVals }

// maxPackedFixed bounds the stored bytes of an exp-fixed3-v1 unit of l
// decoded bytes: its headers, chunks and the most escapes the encoder
// writes.
func maxPackedFixed(l int64) int64 {
	n := l / 2
	return fixedHeaderLen + int64(fixedChunks(int(n)))*fixedChunkLen + n/fixedEscapeShare
}

// EncodeExpFixed3 appends the exp-fixed3-v1 form of src, the little-endian
// BF16 values of one unit, to dst (INF-MP8). Any other dtype, an odd byte
// count, an empty unit and one over 2 MiB is ErrDType; a unit with more than
// one value in eight outside its chunks' exponent windows is refused.
func EncodeExpFixed3(dst, src []byte, dtype string) ([]byte, error) {
	switch {
	case dtype != "BF16":
		return dst, fmt.Errorf("%w: %q; the fixed code takes BF16", ErrDType, dtype)
	case len(src) == 0 || len(src)%2 != 0:
		return dst, fmt.Errorf("%w: %d bytes is not a whole number of 16-bit values", ErrDType, len(src))
	case len(src)/2 > maxChunkValues:
		return dst, fmt.Errorf("%w: unit of %d values exceeds %d", ErrDType, len(src)/2, maxChunkValues)
	}
	n := len(src) / 2
	at := len(dst)
	dst = append(dst, make([]byte, fixedHeaderLen+fixedChunks(n)*fixedChunkLen)...)
	unit := dst[at:]
	var escapes []byte
	for c := range fixedChunks(n) {
		first := c * fixedChunkVals
		vals := min(fixedChunkVals, n-first)
		var hist [256]int
		for v := range vals {
			hist[src[2*(first+v)+1]<<1|src[2*(first+v)]>>7]++
		}
		base, best := 0, -1
		for b := 0; b <= fixedMaxBase; b++ {
			covered := 0
			for e := b; e < b+7; e++ {
				covered += hist[e]
			}
			if covered > best {
				base, best = b, covered
			}
		}
		chunk := unit[fixedHeaderLen+c*fixedChunkLen:][:fixedChunkLen]
		chunk[0] = byte(base)
		binary.LittleEndian.PutUint32(chunk[fixedEscapeAt:], uint32(len(escapes)))
		planes := chunk[fixedChunkHead : fixedChunkHead+3*fixedPlaneLen]
		lo := chunk[fixedChunkHead+3*fixedPlaneLen:]
		for v := range vals {
			x := binary.LittleEndian.Uint16(src[2*(first+v):])
			exp := int(x>>7) & 0xff
			lo[v] = byte(x>>8)&0x80 | byte(x)&0x7f
			code := exp - base
			if code < 0 || code > 6 {
				code = 7
				escapes = append(escapes, byte(exp))
			}
			blk, i, j := v/512, v/64%8, v%64
			for p := range 3 {
				planes[p*fixedPlaneLen+64*blk+j] |= byte(code>>p&1) << i
			}
		}
	}
	if len(escapes) > n/fixedEscapeShare {
		return dst[:at], fmt.Errorf("%w: %d of %d values", errEscapes, len(escapes), n)
	}
	unit[0], unit[1] = fixedVersion, dtypeBF16
	binary.LittleEndian.PutUint32(unit[4:], uint32(n))
	binary.LittleEndian.PutUint32(unit[8:], uint32(len(escapes)))
	return append(dst, escapes...), nil
}

// DecodeExpFixed3 decodes one exp-fixed3-v1 payload into dst, which must be
// exactly the unit's decoded length (INF-MP8). A payload of another size,
// version, dtype or header, a base above 248, a chunk header whose escape
// index is not the count of the earlier chunks' escapes or whose other
// bytes are set, or escapes that do not match the codes is ErrCorrupt; dst
// may then hold partial output.
func DecodeExpFixed3(dst, p []byte) error { return decodeFixed(dst, p, nil) }

// fixedPatch is what a fill needs to apply one exp-fixed3-v1 unit's escapes
// in its hash pass (escapePass) after decodeFixed left each escaped value
// with the exponent b+7, which no value inside its chunk's window has.
type fixedPatch struct {
	bases   []byte // each chunk's window base
	escapes []byte // the unit's escape list
	// offsets holds each escape's value index within its chunk, in value
	// order, as the decode found code 7; starts[c] is chunk c's first
	// escape, the index its header's bytes 4 to 7 hold, and
	// starts[len(bases)] the escape count. The hash pass touches only these
	// values.
	offsets []uint16
	starts  []uint32
	// split is the unit's first value that starts at or after the span
	// boundary inside the unit, its value count when none lies inside.
	split int
	// before counts the escapes of the values before split.
	before int
	// inline is the value whose two bytes lie in two spans, -1 when none;
	// decodeFixed applies its escape in place, since each span's pass
	// writes its own bytes alone.
	inline int
}

// newFixedPatch is the patch of unit c before decoding: where the unit
// crosses a span boundary.
func newFixedPatch(c *ChunkEntry) *fixedPatch {
	d := &fixedPatch{split: int(c.Length / 2), inline: -1}
	boundary := (c.Offset/modelfiles.ChunkSize + 1) * modelfiles.ChunkSize
	if boundary < c.Offset+c.Length {
		rel := boundary - c.Offset
		d.split = int((rel + 1) / 2)
		if rel%2 == 1 {
			d.inline = d.split - 1
		}
	}
	return d
}

// decodeFixed decodes as DecodeExpFixed3 does. With d it leaves each
// escaped value but d.inline at exponent b+7 and fills d's bases, escapes
// and before.
func decodeFixed(dst, p []byte, d *fixedPatch) error {
	n, body, err := fixedHeader(p, len(dst))
	if err != nil {
		return err
	}
	esc := p[body:]
	if d != nil {
		d.bases = make([]byte, fixedChunks(n))
		d.escapes = slices.Clone(esc)
		d.offsets = make([]uint16, 0, len(esc))
		d.starts = make([]uint32, fixedChunks(n)+1)
		defer func() { d.starts[len(d.bases)] = uint32(len(d.offsets)) }()
	}
	k := 0
	for c := range fixedChunks(n) {
		chunk := p[fixedHeaderLen+c*fixedChunkLen:][:fixedChunkLen]
		base, err := fixedChunkHeader(chunk, c, k)
		if err != nil {
			return err
		}
		if d != nil {
			d.bases[c] = base
			d.starts[c] = uint32(k)
		}
		p0 := chunk[fixedChunkHead:][:fixedPlaneLen]
		p1 := chunk[fixedChunkHead+fixedPlaneLen:][:fixedPlaneLen]
		p2 := chunk[fixedChunkHead+2*fixedPlaneLen:][:fixedPlaneLen]
		lo := chunk[fixedChunkHead+3*fixedPlaneLen:][:fixedChunkVals]
		out := dst[2*c*fixedChunkVals:]
		vals := min(fixedChunkVals, n-c*fixedChunkVals)
		b8 := uint64(base) * lanes8
		for blk := range 8 {
			for i := range 8 {
				for jg := 0; jg < 64; jg += 8 {
					v := 512*blk + 64*i + jg
					if v >= vals {
						break
					}
					at := 64*blk + jg
					code := (binary.LittleEndian.Uint64(p0[at:])>>i)&lanes8 |
						(binary.LittleEndian.Uint64(p1[at:])>>i)&lanes8<<1 |
						(binary.LittleEndian.Uint64(p2[at:])>>i)&lanes8<<2
					exp := code + b8
					// A lane holds code 7 when adding 1 sets its bit 3.
					if (code+lanes8)&(lanes8<<3) != 0 {
						for lane := range min(8, vals-v) {
							if code>>(8*lane)&7 == 7 {
								if k == len(esc) {
									return corrupt("chunk %d: more escaped values than the %d escapes", c, len(esc))
								}
								vi := c*fixedChunkVals + v + lane
								if d == nil || vi == d.inline {
									exp = exp&^(0xff<<(8*lane)) | uint64(esc[k])<<(8*lane)
								}
								if d != nil {
									d.offsets = append(d.offsets, uint16(v+lane))
									if vi < d.split {
										d.before++
									}
								}
								k++
							}
						}
					}
					l := binary.LittleEndian.Uint64(lo[v:])
					if v+8 <= vals {
						binary.LittleEndian.PutUint64(out[2*v:], joinFixed(uint32(exp), uint32(l)))
						binary.LittleEndian.PutUint64(out[2*v+8:], joinFixed(uint32(exp>>32), uint32(l>>32)))
						continue
					}
					for lane := range vals - v {
						e, b := uint16(exp>>(8*lane)&0xff), uint16(l>>(8*lane)&0xff)
						binary.LittleEndian.PutUint16(out[2*(v+lane):], b&0x80<<8|e<<7|b&0x7f)
					}
				}
			}
		}
	}
	if k != len(esc) {
		return corrupt("%d escapes for %d escaped values", len(esc), k)
	}
	return nil
}

// fixedHeader checks an exp-fixed3-v1 payload's header and size, and that
// it decodes to l bytes; it returns the value count and where the escapes
// start.
func fixedHeader(p []byte, l int) (n, body int, err error) {
	if len(p) < fixedHeaderLen {
		return 0, 0, corrupt("fixed-code payload of %d bytes is shorter than its header", len(p))
	}
	if p[0] != fixedVersion || p[1] != dtypeBF16 {
		return 0, 0, corrupt("fixed-code version %d, dtype %d", p[0], p[1])
	}
	for _, b := range p[2:4] {
		if b != 0 {
			return 0, 0, corrupt("fixed-code header byte set")
		}
	}
	for _, b := range p[12:fixedHeaderLen] {
		if b != 0 {
			return 0, 0, corrupt("fixed-code header byte set")
		}
	}
	n = int(binary.LittleEndian.Uint32(p[4:]))
	nesc := int(binary.LittleEndian.Uint32(p[8:]))
	if n == 0 || n > maxChunkValues || nesc > n/fixedEscapeShare {
		return 0, 0, corrupt("fixed-code unit of %d values with %d escapes", n, nesc)
	}
	body = fixedHeaderLen + fixedChunks(n)*fixedChunkLen
	if len(p) != body+nesc {
		return 0, 0, corrupt("fixed-code payload of %d bytes, want %d", len(p), body+nesc)
	}
	if l != 2*n {
		return 0, 0, corrupt("unit decodes to %d bytes, the slot holds %d", 2*n, l)
	}
	return n, body, nil
}

// fixedChunkHeader checks chunk c's header, whose escape index must be k,
// and returns its window base.
func fixedChunkHeader(chunk []byte, c, k int) (byte, error) {
	base := chunk[0]
	if base > fixedMaxBase {
		return 0, corrupt("chunk %d window base %d", c, base)
	}
	if at := binary.LittleEndian.Uint32(chunk[fixedEscapeAt:]); at != uint32(k) {
		return 0, corrupt("chunk %d starts at escape %d, its earlier chunks use %d", c, at, k)
	}
	for _, rest := range [][]byte{chunk[1:fixedEscapeAt], chunk[fixedEscapeAt+4 : fixedChunkHead]} {
		for _, b := range rest {
			if b != 0 {
				return 0, corrupt("chunk %d header byte set", c)
			}
		}
	}
	return base, nil
}

// joinFixed builds four BF16 values from four exponent bytes and four
// sign-and-mantissa bytes, one per byte lane.
func joinFixed(e, l uint32) uint64 {
	x, y := spread(e), spread(l)
	return x<<7 | y&0x007f007f007f007f | (y&0x0080008000800080)<<8
}
