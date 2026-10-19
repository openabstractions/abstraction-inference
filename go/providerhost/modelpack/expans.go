package modelpack

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// The exp-ans-v1 unit (inference CONTRACT.md INF-MP11): 16-bit values cut
// into two byte planes, as exp-huffman-v1 cuts them, each plane raw,
// constant, or coded with OA's rANS coder (ans.go) under one static table
// or under one table per context.
//
// A unit is a 16-byte header, then the high plane, then the low plane. The
// header holds the format version 1, the layout, the high and low planes'
// modes, and the value count and the two planes' sizes as little-endian
// 32-bit numbers at bytes 4, 8 and 12. Layouts:
//
//   - 1 BF16 and 2 F16: the values rotated left by one bit, so the high
//     plane is a BF16 value's exponent, as in exp-huffman-v1;
//   - 3 delta words: 16-bit words of a transform against a base, such as
//     int-diff-v1 (DELTA.md), high and low bytes as they are; a plane may be
//     coded under contexts taken from the base: the BF16 exponent of the
//     base value at the same index, minus a window start, clamped to the
//     window's contexts.
//
// Plane modes: 0 raw, 2 constant (one byte), 3 rANS with one table (the
// table, then the stream), 4 rANS with contexts (the window start and the
// number of contexts, at most 32, as two bytes, one table per context,
// absent where no value falls, then the stream). A plane is coded only
// where that saves 3 percent of it.
const (
	ansHeaderLen   = 16
	ansVersion     = 1
	layoutDelta    = 3
	planeANS       = 3
	planeANSCtx    = 4
	ansMaxContexts = 32
)

// EncodeExpANS appends the exp-ans-v1 form of src, the little-endian 16-bit
// values of one unit, to dst. With base nil, dtype is "BF16" or "F16" and
// the values are floats of that type. With base, src holds a transform's
// 16-bit words against base, the unit's BF16 values in the base model, and
// its planes may be coded under the base's exponents; dtype is then
// "BF16". An odd byte count, an empty unit, one over 2 MiB, or a base of
// another length is ErrDType.
func EncodeExpANS(dst, src []byte, dtype string, base []byte) ([]byte, error) {
	kind, ok := dtypeByte(dtype)
	switch {
	case !ok:
		return dst, fmt.Errorf("%w: %q", ErrDType, dtype)
	case len(src) == 0 || len(src)%2 != 0:
		return dst, fmt.Errorf("%w: %d bytes is not a whole number of 16-bit values", ErrDType, len(src))
	case len(src)/2 > maxChunkValues:
		return dst, fmt.Errorf("%w: unit of %d values exceeds %d", ErrDType, len(src)/2, maxChunkValues)
	case base != nil && (len(base) != len(src) || kind != dtypeBF16):
		return dst, fmt.Errorf("%w: a delta's base holds %d bytes of %s for %d", ErrDType, len(base), dtype, len(src))
	}
	n := len(src) / 2
	hi, lo := make([]byte, n), make([]byte, n)
	layout := kind
	if base != nil {
		layout = layoutDelta
	}
	for i := range n {
		v := binary.LittleEndian.Uint16(src[2*i:])
		if base == nil {
			v = v<<1 | v>>15
		}
		hi[i], lo[i] = byte(v>>8), byte(v)
	}
	at := len(dst)
	dst = append(dst, make([]byte, ansHeaderLen)...)
	start := len(dst)
	dst, hiMode := appendANSPlane(dst, hi, base)
	hiLen := len(dst) - start
	dst, loMode := appendANSPlane(dst, lo, base)
	loLen := len(dst) - start - hiLen
	h := dst[at : at+ansHeaderLen]
	h[0], h[1], h[2], h[3] = ansVersion, layout, hiMode, loMode
	binary.LittleEndian.PutUint32(h[4:], uint32(n))
	binary.LittleEndian.PutUint32(h[8:], uint32(hiLen))
	binary.LittleEndian.PutUint32(h[12:], uint32(loLen))
	return dst, nil
}

// baseExponent is the BF16 exponent of base value i.
func baseExponent(base []byte, i int) int {
	return int(base[2*i+1]&0x7f)<<1 | int(base[2*i]>>7)
}

// appendANSPlane appends a plane in the smallest of its modes: constant, raw,
// one table, or (with a base) contexts; a coded mode must save 3 percent.
func appendANSPlane(dst, plane, base []byte) ([]byte, byte) {
	var hist [256]uint64
	for _, b := range plane {
		hist[b]++
	}
	distinct := 0
	for _, c := range hist {
		if c > 0 {
			distinct++
		}
	}
	if distinct == 1 {
		return append(dst, plane[0]), planeConst
	}
	n := len(plane)
	limit := n - n*3/100
	best, mode := []byte(nil), byte(planeRaw)
	var t ansTable
	if normalize(&hist, &t) {
		p := appendTable(nil, &t)
		p = ansEncode(p, plane, nil, []*ansTable{&t})
		if len(p) <= limit {
			best, mode = p, planeANS
		}
	}
	if base != nil {
		if p, ok := ctxPlane(plane, base); ok && len(p) <= limit && (best == nil || len(p) < len(best)) {
			best, mode = p, planeANSCtx
		}
	}
	if best == nil {
		return append(dst, plane...), planeRaw
	}
	return append(dst, best...), mode
}

// ctxPlane codes plane under the base's exponents: the window of at most
// ansMaxContexts exponents holding the most values, one table per exponent
// in it, values outside it counted at its nearer end.
func ctxPlane(plane, base []byte) ([]byte, bool) {
	var eh [256]int
	for i := range plane {
		eh[baseExponent(base, i)]++
	}
	cmin, bestCount := 0, -1
	for c := 0; c+ansMaxContexts <= 256; c++ {
		count := 0
		for e := c; e < c+ansMaxContexts; e++ {
			count += eh[e]
		}
		if count > bestCount {
			cmin, bestCount = c, count
		}
	}
	// Trim the window to the exponents present, so no table is wasted at
	// either end.
	lo, hi := cmin, cmin+ansMaxContexts-1
	for lo < hi && eh[lo] == 0 {
		lo++
	}
	for hi > lo && eh[hi] == 0 {
		hi--
	}
	cmin = lo
	contexts := hi - lo + 1
	ctx := make([]byte, len(plane))
	hists := make([][256]uint64, contexts)
	for i, b := range plane {
		c := byte(min(max(baseExponent(base, i)-cmin, 0), contexts-1))
		ctx[i] = c
		hists[c][b]++
	}
	tabs := make([]*ansTable, contexts)
	p := []byte{byte(cmin), byte(contexts)}
	for c := range tabs {
		var t ansTable
		if normalize(&hists[c], &t) {
			tabs[c] = &t
		}
		p = appendTable(p, tabs[c])
	}
	return ansEncode(p, plane, ctx, tabs), true
}

var planePool = sync.Pool{New: func() any { b := make([]byte, 0, maxChunkValues); return &b }}

// slotsPool holds decoding tables for one plane, reused plane after plane.
var slotsPool = sync.Pool{New: func() any { return new([ansMaxContexts]ansSlots) }}

// DecodeExpANS decodes one exp-ans-v1 payload into dst, which must be
// exactly the unit's decoded length (INF-MP11). A delta-words unit needs
// base, the unit's BF16 values in the base model, of the same length; it
// decodes to the transform's words. A payload of another size, version,
// layout or mode, a plane that does not decode as described, or a delta
// unit without its base is ErrCorrupt; dst may then hold partial output.
func DecodeExpANS(dst, p, base []byte) error { return decodeExpANS(dst, p, base, false) }

// decodeExpANS decodes as DecodeExpANS does; with undo, a delta unit's
// words are turned back into the fine-tune's values against base as the
// planes join (DecodeDelta).
func decodeExpANS(dst, p, base []byte, undo bool) error {
	if len(p) < ansHeaderLen {
		return corrupt("ans payload of %d bytes is shorter than its header", len(p))
	}
	layout, hiMode, loMode := p[1], p[2], p[3]
	n := int(binary.LittleEndian.Uint32(p[4:]))
	hiLen := int(binary.LittleEndian.Uint32(p[8:]))
	loLen := int(binary.LittleEndian.Uint32(p[12:]))
	switch {
	case p[0] != ansVersion:
		return corrupt("ans version %d", p[0])
	case layout != dtypeBF16 && layout != dtypeF16 && layout != layoutDelta:
		return corrupt("ans layout %d", layout)
	case n == 0 || n > maxChunkValues || len(dst) != 2*n:
		return corrupt("ans unit of %d values for a slot of %d bytes", n, len(dst))
	case hiLen < 0 || loLen < 0 || ansHeaderLen+hiLen+loLen != len(p):
		return corrupt("ans planes of %d and %d bytes in a %d-byte payload", hiLen, loLen, len(p))
	case layout == layoutDelta && len(base) != len(dst):
		return corrupt("a delta unit needs its base's %d bytes, given %d", len(dst), len(base))
	}
	hb, lb := planePool.Get().(*[]byte), planePool.Get().(*[]byte)
	defer planePool.Put(hb)
	defer planePool.Put(lb)
	hi, lo := (*hb)[:n], (*lb)[:n]
	// A delta's contexts come from its base's exponents, taken once for
	// both planes.
	sp := slotsPool.Get().(*[ansMaxContexts]ansSlots)
	defer slotsPool.Put(sp)
	var exp []byte
	if layout == layoutDelta {
		eb := planePool.Get().(*[]byte)
		defer planePool.Put(eb)
		exp = (*eb)[:n]
		baseExponents(exp, base)
	}
	hi, err := decodeANSPlane(hi, p[ansHeaderLen:ansHeaderLen+hiLen], hiMode, layout, exp, sp)
	if err != nil {
		return fmt.Errorf("high plane: %w", err)
	}
	lo, err = decodeANSPlane(lo, p[ansHeaderLen+hiLen:], loMode, layout, exp, sp)
	if err != nil {
		return fmt.Errorf("low plane: %w", err)
	}
	if undo {
		joinIntDiff(dst, hi, lo, base)
		return nil
	}
	joinPlanes(dst, hi, lo, layout != layoutDelta)
	return nil
}

// joinPlanes writes value i from hi[i] and lo[i], rotated back right by one
// bit when rotate is set; four values per 64-bit word.
func joinPlanes(dst, hi, lo []byte, rotate bool) {
	n := len(hi)
	i := 0
	for ; i+4 <= n; i += 4 {
		x := spread(binary.LittleEndian.Uint32(hi[i:]))<<8 | spread(binary.LittleEndian.Uint32(lo[i:]))
		if rotate {
			x = (x>>1)&0x7fff7fff7fff7fff | (x&0x0001000100010001)<<15
		}
		binary.LittleEndian.PutUint64(dst[2*i:], x)
	}
	for ; i < n; i++ {
		r := uint16(hi[i])<<8 | uint16(lo[i])
		if rotate {
			r = r>>1 | r<<15
		}
		binary.LittleEndian.PutUint16(dst[2*i:], r)
	}
}

// baseExponents writes each base value's BF16 exponent into exp, four
// values per 64-bit word.
func baseExponents(exp, base []byte) {
	n := len(exp)
	i := 0
	for ; i+4 <= n; i += 4 {
		e := binary.LittleEndian.Uint64(base[2*i:]) >> 7 & 0x00ff00ff00ff00ff
		exp[i], exp[i+1], exp[i+2], exp[i+3] = byte(e), byte(e>>16), byte(e>>32), byte(e>>48)
	}
	for ; i < n; i++ {
		exp[i] = byte(baseExponent(base, i))
	}
}

// decodeANSPlane decodes one plane into out and returns the plane's bytes:
// out, or p itself for a raw plane. exp holds a delta's base exponents,
// and sp is room for the plane's decoding tables.
func decodeANSPlane(out, p []byte, mode, layout byte, exp []byte, sp *[ansMaxContexts]ansSlots) ([]byte, error) {
	switch mode {
	case planeRaw:
		if len(p) != len(out) {
			return nil, corrupt("raw plane of %d bytes for %d values", len(p), len(out))
		}
		return p, nil
	case planeConst:
		if len(p) != 1 {
			return nil, corrupt("constant plane of %d bytes", len(p))
		}
		for i := range out {
			out[i] = p[0]
		}
		return out, nil
	case planeANS:
		slots, at, err := readTableInto(p, &sp[0])
		if err != nil {
			return nil, err
		}
		if slots == nil {
			return nil, corrupt("ans plane without a table")
		}
		return out, ansDecode0(out, p[at:], slots)
	case planeANSCtx:
		if layout != layoutDelta {
			return nil, corrupt("context plane in layout %d", layout)
		}
		if len(p) < 2 || p[1] == 0 || int(p[1]) > ansMaxContexts || int(p[0])+int(p[1]) > 256 {
			return nil, corrupt("ans contexts %v", p[:min(2, len(p))])
		}
		cmin, contexts := int(p[0]), int(p[1])
		var slots [ansMaxContexts]*ansSlots
		at := 2
		for c := range contexts {
			s, k, err := readTableInto(p[at:], &sp[c])
			if err != nil {
				return nil, err
			}
			slots[c] = s
			at += k
		}
		var byExp [256]*ansSlots
		for e := range byExp {
			byExp[e] = slots[min(max(e-cmin, 0), contexts-1)]
		}
		return out, ansDecodeCtx(out, p[at:], &byExp, exp)
	}
	return nil, corrupt("ans plane mode %d", mode)
}
