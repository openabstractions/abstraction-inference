package modelpack

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// The exp-huffman-v1 chunk (inference CONTRACT.md INF-MP3).
//
// Each 16-bit value is rotated left by one bit. The high byte of a rotated
// BF16 value is its 8-bit exponent; the low byte is its 7 mantissa bits and
// its sign. A rotated F16 value carries its 5-bit exponent and 3 mantissa
// bits high, 7 mantissa bits and its sign low. The chunk stores the high
// bytes of all values as one plane and the low bytes as another, each raw,
// constant, or Huffman-coded with its own canonical code of at most 12 bits.
//
// A coded plane is split into 256 substreams: substream t holds the symbols
// of the value quads t, t+256, t+512 and so on, four symbols per quad. Each
// substream starts on a byte and its end offset is stored, so a decoder can
// start at any substream, and many decoders (CPU lanes or GPU threads) write
// neighbouring quads side by side.
const (
	headerLen   = 16
	substreams  = 256
	lensLen     = substreams / 2 // 256 code lengths, two per byte
	endsLen     = substreams * 4 // 256 little-endian uint32 end offsets
	codedPrefix = lensLen + endsLen

	formatVersion = 1

	// maxChunkValues bounds one chunk at 2 MiB of 16-bit values.
	maxChunkValues = 1 << 20

	planeRaw     = 0
	planeHuffman = 1
	planeConst   = 2

	dtypeBF16 = 1
	dtypeF16  = 2

	// lanes is how many neighbouring substreams the CPU decoder advances in
	// lockstep; their four-value quads fill one 64-byte line of output.
	lanes = 8
)

func dtypeByte(dtype string) (byte, bool) {
	switch dtype {
	case "BF16":
		return dtypeBF16, true
	case "F16":
		return dtypeF16, true
	}
	return 0, false
}

// EncodeChunk appends the exp-huffman-v1 form of src, the little-endian
// 16-bit floats of one chunk, to dst. dtype is "BF16" or "F16" (the names
// safetensors and GGUF use); any other dtype, an odd byte count, an empty
// chunk or one over 2 MiB is ErrDType. Each plane is coded only where its
// code saves at least 3 percent of the plane.
func EncodeChunk(dst, src []byte, dtype string) ([]byte, error) {
	kind, ok := dtypeByte(dtype)
	switch {
	case !ok:
		return dst, fmt.Errorf("%w: %q", ErrDType, dtype)
	case len(src) == 0 || len(src)%2 != 0:
		return dst, fmt.Errorf("%w: %d bytes is not a whole number of 16-bit values", ErrDType, len(src))
	case len(src)/2 > maxChunkValues:
		return dst, fmt.Errorf("%w: chunk of %d values exceeds %d", ErrDType, len(src)/2, maxChunkValues)
	}
	n := len(src) / 2
	hi := make([]byte, n)
	lo := make([]byte, n)
	for i := range n {
		v := binary.LittleEndian.Uint16(src[2*i:])
		r := v<<1 | v>>15
		hi[i], lo[i] = byte(r>>8), byte(r)
	}
	at := len(dst)
	dst = append(dst, make([]byte, headerLen)...)
	start := len(dst)
	dst, hiMode := appendPlane(dst, hi)
	hiLen := len(dst) - start
	dst, loMode := appendPlane(dst, lo)
	loLen := len(dst) - start - hiLen
	h := dst[at : at+headerLen]
	h[0], h[1], h[2], h[3] = formatVersion, kind, hiMode, loMode
	binary.LittleEndian.PutUint32(h[4:], uint32(n))
	binary.LittleEndian.PutUint32(h[8:], uint32(hiLen))
	binary.LittleEndian.PutUint32(h[12:], uint32(loLen))
	return dst, nil
}

// appendPlane appends one byte plane as a constant, a Huffman code that
// saves at least 3 percent, or raw bytes.
func appendPlane(dst, plane []byte) ([]byte, byte) {
	var freq [256]uint64
	for _, b := range plane {
		freq[b]++
	}
	distinct := 0
	for _, c := range freq {
		if c > 0 {
			distinct++
		}
	}
	if distinct == 1 {
		return append(dst, plane[0]), planeConst
	}
	n := len(plane)
	limit := n - n*3/100
	var lens [256]uint8
	codeLengths(&freq, &lens)
	bits := uint64(0)
	for s, c := range freq {
		bits += c * uint64(lens[s])
	}
	if codedPrefix+int(bits/8)+substreams > limit {
		return append(dst, plane...), planeRaw
	}
	at := len(dst)
	dst = appendCoded(dst, plane, &lens)
	if len(dst)-at > limit {
		return append(dst[:at], plane...), planeRaw
	}
	return dst, planeHuffman
}

func appendCoded(dst, plane []byte, lens *[256]uint8) []byte {
	var codes [256]uint16
	canonicalCodes(lens, &codes)
	for i := 0; i < substreams; i += 2 {
		dst = append(dst, lens[i]|lens[i+1]<<4)
	}
	endsAt := len(dst)
	dst = append(dst, make([]byte, endsLen)...)
	data := len(dst)
	n := len(plane)
	quads := (n + 3) / 4
	for t := range substreams {
		var acc uint64
		var nb uint
		for q := t; q < quads; q += substreams {
			for i := 4 * q; i < min(4*q+4, n); i++ {
				s := plane[i]
				acc |= uint64(codes[s]) << nb
				nb += uint(lens[s])
				if nb >= 32 {
					dst = binary.LittleEndian.AppendUint32(dst, uint32(acc))
					acc >>= 32
					nb -= 32
				}
			}
		}
		for ; nb > 0; nb -= min(nb, 8) {
			dst = append(dst, byte(acc))
			acc >>= 8
		}
		binary.LittleEndian.PutUint32(dst[endsAt+4*t:], uint32(len(dst)-data))
	}
	return dst
}

// plane is one parsed byte plane of a chunk.
type plane struct {
	mode   byte
	raw    []byte // planeRaw: one byte per value
	value  byte   // planeConst
	tables *tables
	tab    *decodeTable
	pairs  *pairTable
	// paired: decode two symbols per lookup (buildPairs).
	paired bool
	data   []byte                 // planeHuffman: the substreams
	start  [substreams + 1]uint32 // substream t is data[start[t]:start[t+1]]
}

// chunk is one parsed exp-huffman-v1 payload.
type chunk struct {
	count  int
	hi, lo plane
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrCorrupt}, args...)...)
}

// DecodeChunk decodes one exp-huffman-v1 payload into dst, which must be
// exactly the chunk's decoded length. A payload that is truncated, carries
// trailing bytes, names an unknown version, dtype or plane mode, holds an
// incomplete code, or whose substreams do not end where their offsets say is
// ErrCorrupt; dst may then hold partial output.
func DecodeChunk(dst, payload []byte) error {
	var c chunk
	defer c.release()
	if err := c.parse(payload); err != nil {
		return err
	}
	if 2*c.count != len(dst) {
		return corrupt("chunk decodes to %d bytes, the slot holds %d", 2*c.count, len(dst))
	}
	return c.decode(dst, 0, substreams)
}

// tables are one coded plane's decoding tables, 24 KiB, reused across
// chunks through tablePool.
type tables struct {
	single decodeTable
	pairs  pairTable
}

var tablePool = sync.Pool{New: func() any { return new(tables) }}

// release returns the chunk's tables to the pool; the chunk cannot decode
// afterwards.
func (c *chunk) release() {
	for _, pl := range []*plane{&c.hi, &c.lo} {
		if pl.tables != nil {
			tablePool.Put(pl.tables)
			pl.tables, pl.tab, pl.pairs = nil, nil, nil
		}
	}
}

func (c *chunk) parse(p []byte) error {
	if len(p) < headerLen {
		return corrupt("payload of %d bytes is shorter than its header", len(p))
	}
	if p[0] != formatVersion {
		return corrupt("version %d", p[0])
	}
	if p[1] != dtypeBF16 && p[1] != dtypeF16 {
		return corrupt("dtype %d", p[1])
	}
	n := binary.LittleEndian.Uint32(p[4:])
	hiLen := uint64(binary.LittleEndian.Uint32(p[8:]))
	loLen := uint64(binary.LittleEndian.Uint32(p[12:]))
	if n == 0 || n > maxChunkValues {
		return corrupt("value count %d", n)
	}
	if headerLen+hiLen+loLen != uint64(len(p)) {
		return corrupt("planes of %d and %d bytes in a payload of %d", hiLen, loLen, len(p))
	}
	c.count = int(n)
	if err := c.hi.parse(p[2], p[headerLen:headerLen+hiLen], c.count, true); err != nil {
		return err
	}
	return c.lo.parse(p[3], p[headerLen+hiLen:], c.count, false)
}

func (pl *plane) parse(mode byte, b []byte, n int, high bool) error {
	pl.mode = mode
	switch mode {
	case planeRaw:
		if len(b) != n {
			return corrupt("raw plane of %d bytes for %d values", len(b), n)
		}
		pl.raw = b
	case planeConst:
		if len(b) != 1 {
			return corrupt("constant plane of %d bytes", len(b))
		}
		pl.value = b[0]
	case planeHuffman:
		if len(b) < codedPrefix {
			return corrupt("coded plane of %d bytes is shorter than its tables", len(b))
		}
		var lens [256]uint8
		for i := range lensLen {
			lens[2*i], lens[2*i+1] = b[i]&15, b[i]>>4
		}
		pl.tables = tablePool.Get().(*tables)
		pl.tab, pl.pairs = &pl.tables.single, &pl.tables.pairs
		if !buildTable(&lens, pl.tab) {
			return corrupt("code lengths are not a complete code of at most %d bits", maxCodeLen)
		}
		pl.paired = buildPairs(pl.tab, pl.pairs, high)
		pl.data = b[codedPrefix:]
		prev := uint32(0)
		for t := range substreams {
			end := binary.LittleEndian.Uint32(b[lensLen+4*t:])
			if end < prev {
				return corrupt("substream %d ends before it starts", t)
			}
			pl.start[t+1] = end
			prev = end
		}
		if uint64(prev) != uint64(len(pl.data)) {
			return corrupt("substreams end at %d of %d bytes", prev, len(pl.data))
		}
	default:
		return corrupt("plane mode %d", mode)
	}
	return nil
}

func load64(b []byte, i uint64) uint64 {
	if i+8 <= uint64(len(b)) {
		return binary.LittleEndian.Uint64(b[i:])
	}
	var v uint64
	for k := uint64(0); i+k < uint64(len(b)) && k < 8; k++ {
		v |= uint64(b[i+k]) << (8 * k)
	}
	return v
}

// get4 returns the plane's next cnt symbols of quad q, one per 16-bit lane.
// p is the bit position in a coded plane's substream of q.
func (pl *plane) get4(q, cnt int, p *uint64) uint64 {
	var v uint64
	switch pl.mode {
	case planeRaw:
		for i := range cnt {
			v |= uint64(pl.raw[4*q+i]) << (16 * i)
		}
	case planeConst:
		for i := range cnt {
			v |= uint64(pl.value) << (16 * i)
		}
	default:
		bits := load64(pl.data, *p>>3) >> (*p & 7)
		for i := range cnt {
			e := pl.tab[bits&tableMask]
			v |= uint64(e&0xff) << (16 * i)
			bits >>= (e >> 8) & 15
			*p += uint64((e >> 8) & 15)
		}
	}
	return v
}

// unrotate joins high and low bytes, one value per 16-bit lane, and rotates
// each lane right by one bit.
func unrotate(h, l uint64) uint64 {
	x := h<<8 | l
	return (x>>1)&0x7fff7fff7fff7fff | (x&0x0001000100010001)<<15
}

func spread(b uint32) uint64 {
	v := uint64(b)
	v = (v | v<<16) & 0x0000ffff0000ffff
	return (v | v<<8) & 0x00ff00ff00ff00ff
}

// fullQuads counts the whole quads below nFull in substream t.
func fullQuads(t, nFull int) int {
	if t >= nFull {
		return 0
	}
	return (nFull-1-t)/substreams + 1
}

// decode writes the quads of substreams [a, b) into dst. Decoders given
// disjoint ranges write disjoint bytes and may run side by side.
func (c *chunk) decode(dst []byte, a, b int) error {
	var pos [2][substreams]uint64
	for t := a; t < b; t++ {
		pos[0][t] = uint64(c.hi.start[t]) * 8
		pos[1][t] = uint64(c.lo.start[t]) * 8
	}
	if c.hi.mode == planeHuffman && c.lo.mode != planeConst {
		c.decodeLanes(dst, a, b, &pos)
	} else {
		c.decodeGeneric(dst, a, b, &pos)
	}
	// The final partial quad, if any, is the last quad of its substream.
	nFull, rest := c.count/4, c.count%4
	if t := nFull % substreams; rest != 0 && t >= a && t < b {
		x := unrotate(c.hi.get4(nFull, rest, &pos[0][t]), c.lo.get4(nFull, rest, &pos[1][t]))
		for i := range rest {
			binary.LittleEndian.PutUint16(dst[8*nFull+2*i:], uint16(x>>(16*i)))
		}
	}
	for k, pl := range [2]*plane{&c.hi, &c.lo} {
		if pl.mode != planeHuffman {
			continue
		}
		for t := a; t < b; t++ {
			if (pos[k][t]+7)/8 != uint64(pl.start[t+1]) {
				return corrupt("substream %d of the %s plane ends at bit %d, its bytes end at %d",
					t, [2]string{"high", "low"}[k], pos[k][t]-uint64(pl.start[t])*8, pl.start[t+1]-pl.start[t])
			}
		}
	}
	return nil
}

func (c *chunk) decodeGeneric(dst []byte, a, b int, pos *[2][substreams]uint64) {
	nFull := c.count / 4
	for t := a; t < b; t++ {
		for q := t; q < nFull; q += substreams {
			c.decodeQuad(dst, q, t, pos)
		}
	}
}

// pairTable is indexed like decodeTable and decodes two symbols at once
// when their codes fit in 12 bits together. A high-plane entry holds the
// symbols at bits 7 and 23, where they sit in two output values, the
// joint length in bits 0-3 and pairHi when it holds a pair. A low-plane
// entry holds them at bits 0 and 16, the length in bits 8-11 and pairLo.
type pairTable [tableSize]uint32

const (
	pairHi     = 0x10
	pairLo     = 0x1000
	pairHiSyms = 0x7f807f80
	pairLoSyms = 0x00ff00ff
)

// buildPairs fills pairs and reports whether pairs pay: whether, for symbols
// drawn as the code expects, both lookups of a quad find a pair at least
// 15 times in 16. Otherwise the decoder takes four single lookups, whose
// path has no branch to mispredict.
func buildPairs(tab *decodeTable, pairs *pairTable, high bool) bool {
	hits := 0
	for i := range tableSize {
		e1 := tab[i]
		l1 := e1 >> 8
		e2 := tab[i>>l1]
		l := uint32(l1 + e2>>8)
		switch {
		case l > maxCodeLen:
			pairs[i] = 0
			continue
		case high:
			pairs[i] = uint32(e1&0xff)<<7 | uint32(e2&0xff)<<23 | l | pairHi
		default:
			pairs[i] = uint32(e1&0xff) | uint32(e2&0xff)<<16 | l<<8 | pairLo
		}
		hits++
	}
	// Each index is as likely as the code assumes its symbols are.
	p := float64(hits) / tableSize
	return p*p >= 15.0/16
}

// quad decodes four symbols one at a time from the bits v, for a quad whose
// codes do not pair; it returns one symbol per 16-bit lane and the bits used.
func quad(tab *decodeTable, v uint64) (uint64, uint64) {
	e0 := tab[v&tableMask]
	v >>= (e0 >> 8) & 15
	e1 := tab[v&tableMask]
	v >>= (e1 >> 8) & 15
	e2 := tab[v&tableMask]
	v >>= (e2 >> 8) & 15
	e3 := tab[v&tableMask]
	return uint64(e0&0xff) | uint64(e1&0xff)<<16 | uint64(e2&0xff)<<32 | uint64(e3&0xff)<<48,
		uint64((e0>>8)&15 + (e1>>8)&15 + (e2>>8)&15 + (e3>>8)&15)
}

// lowBits places low-plane bytes, one per 16-bit lane, where the rotation
// puts them: bits 1-7 down to 0-6 and bit 0, the sign, up to 15.
func lowBits(l uint64) uint64 {
	return (l>>1)&0x007f007f007f007f | (l&0x0001000100010001)<<15
}

// decodeLanes is the path BF16 and F16 take: a coded high plane and a raw or
// coded low plane. Round k decodes quad k of every substream in [a, b),
// whose quads are neighbours, so dst is written in order. It advances
// `lanes` neighbouring substreams at a time; their decoding chains are
// independent, so the core overlaps them, and their quads fill one 64-byte
// line of dst.
func (c *chunk) decodeLanes(dst []byte, a, b int, pos *[2][substreams]uint64) {
	hiTab, hiPairs, hiData := c.hi.tab, c.hi.pairs, c.hi.data
	loTab, loPairs, loData, raw := c.lo.tab, c.lo.pairs, c.lo.data, c.lo.raw
	loCoded := c.lo.mode == planeHuffman
	hiPaired, loPaired := c.hi.paired, c.lo.paired
	nFull := c.count / 4
	// Every substream in [a, b) holds `rounds` whole quads; lower ones may
	// hold one more.
	rounds := fullQuads(b-1, nFull)
	for k := range rounds {
		t := a
		for ; t+lanes <= b; t += lanes {
			ph := (*[lanes]uint64)(pos[0][t : t+lanes])
			pl := (*[lanes]uint64)(pos[1][t : t+lanes])
			q0 := t + k*substreams
			out := dst[8*q0 : 8*q0+8*lanes]
			for j := range lanes {
				p := ph[j]
				v := load64(hiData, p>>3) >> (p & 7)
				var h uint64
				x, y := uint32(0), uint32(0)
				if hiPaired {
					x = hiPairs[v&tableMask]
					y = hiPairs[(v>>(x&15))&tableMask]
				}
				if x&y&pairHi != 0 {
					h = uint64(x&pairHiSyms) | uint64(y&pairHiSyms)<<32
					ph[j] = p + uint64(x&15+y&15)
				} else {
					s, n := quad(hiTab, v)
					h = s << 7
					ph[j] = p + n
				}
				var l uint64
				if loCoded {
					p := pl[j]
					v := load64(loData, p>>3) >> (p & 7)
					x, y := uint32(0), uint32(0)
					if loPaired {
						x = loPairs[v&tableMask]
						y = loPairs[(v>>((x>>8)&15))&tableMask]
					}
					if x&y&pairLo != 0 {
						l = uint64(x&pairLoSyms) | uint64(y&pairLoSyms)<<32
						pl[j] = p + uint64((x>>8)&15+(y>>8)&15)
					} else {
						s, n := quad(loTab, v)
						l = s
						pl[j] = p + n
					}
				} else {
					l = spread(binary.LittleEndian.Uint32(raw[4*(q0+j):]))
				}
				binary.LittleEndian.PutUint64(out[8*j:], h|lowBits(l))
			}
		}
		for ; t < b; t++ {
			c.decodeQuad(dst, t+k*substreams, t, pos)
		}
	}
	for t := a; t < b; t++ {
		if q := t + rounds*substreams; q < nFull {
			c.decodeQuad(dst, q, t, pos)
		}
	}
}

// decodeQuad decodes whole quad q of substream t on the generic path.
func (c *chunk) decodeQuad(dst []byte, q, t int, pos *[2][substreams]uint64) {
	x := unrotate(c.hi.get4(q, 4, &pos[0][t]), c.lo.get4(q, 4, &pos[1][t]))
	binary.LittleEndian.PutUint64(dst[8*q:], x)
}
