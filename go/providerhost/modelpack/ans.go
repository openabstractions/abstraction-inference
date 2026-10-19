package modelpack

import (
	"encoding/binary"
	"math"
)

// OA's static rANS coder, written in house (VISION 2026-10-02), after
// Jarek Duda's asymmetric numeral systems ("Asymmetric numeral systems:
// entropy coding combining speed of Huffman coding with compression rate
// of arithmetic coding", arXiv:1311.2540, 2013), the rANS layout of Fabian
// Giesen's public-domain ryg_rans (https://github.com/rygorous/ryg_rans),
// and the interleaving of several states over one stream from Giesen's
// "Interleaved entropy coders" (arXiv:1402.3392, 2014). No code is taken
// from them.
//
// A coded plane holds bytes. Each table gives every symbol a frequency out
// of ansScale (12 bits); a state is 32 bits kept in [ansL, ansL<<16) and
// moves 16-bit words. ansStates states take symbols in turn (symbol i uses
// state i mod ansStates), so a decoder keeps four independent chains in
// flight on one core; units decode in parallel across cores. The encoder
// runs backwards and writes the words a decoder reads forwards: first each
// state's 32 bits, low word first, state 0 first, then the renormalisation
// words in the order the decoder needs them. Every state starts at ansL,
// so a decoder that ends anywhere else, or with words left over, has read
// a stream no encoder wrote.
const (
	ansScaleBits = 12
	ansScale     = 1 << ansScaleBits
	ansL         = 1 << 16
	ansStates    = 4
)

// ansTable is one normalised frequency table: freq sums to ansScale, and
// cum[s] is the sum of freq below s.
type ansTable struct {
	freq [256]uint32
	cum  [256]uint32
}

// ansSlots is a table's decoding form: for each slot of ansScale, its
// symbol, its frequency minus one and its symbol's start, packed as
// sym | (freq-1)<<8 | start<<20.
type ansSlots [ansScale]uint32

// normalize scales a histogram to ansScale, keeping every present symbol
// at least 1; it reports false for an empty histogram.
func normalize(hist *[256]uint64, t *ansTable) bool {
	var total uint64
	for _, c := range hist {
		total += c
	}
	if total == 0 {
		return false
	}
	sum := uint32(0)
	for s, c := range hist {
		t.freq[s] = 0
		if c > 0 {
			f := uint32(c * ansScale / total)
			t.freq[s] = max(f, 1)
			sum += t.freq[s]
		}
	}
	// Move the rounding error onto the largest frequencies, keeping each
	// present symbol at 1 or more.
	for sum != ansScale {
		big := 0
		for s := range t.freq {
			if t.freq[s] > t.freq[big] {
				big = s
			}
		}
		if sum > ansScale {
			cut := min(sum-ansScale, t.freq[big]-1)
			if cut == 0 { // every present symbol is at 1: more symbols than slots
				return false
			}
			t.freq[big] -= cut
			sum -= cut
		} else {
			t.freq[big] += ansScale - sum
			sum = ansScale
		}
	}
	c := uint32(0)
	for s := range t.cum {
		t.cum[s] = c
		c += t.freq[s]
	}
	return true
}

// cost estimates the coded size of hist under t, in bits.
func (t *ansTable) cost(hist *[256]uint64) float64 {
	b := 0.0
	for s, c := range hist {
		if c > 0 {
			b += float64(c) * (ansScaleBits - math.Log2(float64(t.freq[s])))
		}
	}
	return b
}

// appendTable writes t: for each present symbol, the count of absent
// symbols before it and its frequency minus one, as unsigned varints,
// then the count of absent symbols to the end. An absent table is one
// run of 256.
func appendTable(dst []byte, t *ansTable) []byte {
	run := uint64(0)
	for s := range 256 {
		if t == nil || t.freq[s] == 0 {
			run++
			continue
		}
		dst = binary.AppendUvarint(dst, run)
		dst = binary.AppendUvarint(dst, uint64(t.freq[s]-1))
		run = 0
	}
	if run > 0 {
		dst = binary.AppendUvarint(dst, run)
	}
	return dst
}

// readTable reads one table from p into a new slots array; see
// readTableInto.
func readTable(p []byte) (*ansSlots, int, error) {
	return readTableInto(p, new(ansSlots))
}

// readTableInto reads one table from p into slots and returns slots and
// the bytes read; it returns nil slots for an absent table. A table whose
// frequencies do not sum to ansScale, or whose runs pass 256 symbols, is
// ErrCorrupt.
func readTableInto(p []byte, slots *ansSlots) (*ansSlots, int, error) {
	var freq [256]uint32
	at, s, sum := 0, 0, uint32(0)
	for s < 256 {
		run, n := binary.Uvarint(p[at:])
		if n <= 0 || run > uint64(256-s) {
			return nil, 0, corrupt("ans table: bad run at symbol %d", s)
		}
		at += n
		s += int(run)
		if s == 256 {
			break
		}
		f, n := binary.Uvarint(p[at:])
		if n <= 0 || f >= ansScale {
			return nil, 0, corrupt("ans table: bad frequency at symbol %d", s)
		}
		at += n
		freq[s] = uint32(f) + 1
		sum += freq[s]
		if sum > ansScale {
			return nil, 0, corrupt("ans table: frequencies pass %d", ansScale)
		}
		s++
	}
	if sum == 0 {
		return nil, at, nil
	}
	if sum != ansScale {
		return nil, 0, corrupt("ans table: frequencies sum to %d, want %d", sum, ansScale)
	}
	start := uint32(0)
	for sym := range 256 {
		f := freq[sym]
		for i := start; i < start+f; i++ {
			slots[i] = uint32(sym) | (f-1)<<8 | start<<20
		}
		start += f
	}
	return slots, at, nil
}

// ansEncode codes syms, symbol i with tabs[ctx[i]] (ctx nil: tabs[0]), and
// appends the stream of little-endian 16-bit words to dst.
func ansEncode(dst, syms, ctx []byte, tabs []*ansTable) []byte {
	var x [ansStates]uint32
	for j := range x {
		x[j] = ansL
	}
	words := make([]uint16, 0, len(syms)/4+2*ansStates)
	for i := len(syms) - 1; i >= 0; i-- {
		t := tabs[0]
		if ctx != nil {
			t = tabs[ctx[i]]
		}
		s := syms[i]
		f := t.freq[s]
		st := &x[i%ansStates]
		// In 64 bits: a symbol holding the whole table (f = ansScale) has
		// the bound 2^32, which no state reaches.
		if uint64(*st) >= uint64((ansL>>ansScaleBits)<<16)*uint64(f) {
			words = append(words, uint16(*st))
			*st >>= 16
		}
		*st = (*st/f)<<ansScaleBits + *st%f + t.cum[s]
	}
	for j := ansStates - 1; j >= 0; j-- {
		words = append(words, uint16(x[j]>>16), uint16(x[j]))
	}
	for i := len(words) - 1; i >= 0; i-- {
		dst = binary.LittleEndian.AppendUint16(dst, words[i])
	}
	return dst
}

// ansStream reads the states and returns them with the position of the
// first renormalisation word.
func ansStream(w []byte) ([ansStates]uint32, int, error) {
	var x [ansStates]uint32
	if len(w)%2 != 0 || len(w) < 4*ansStates {
		return x, 0, corrupt("ans stream of %d bytes", len(w))
	}
	for j := range x {
		x[j] = binary.LittleEndian.Uint32(w[4*j:])
		// An encoder's states end in [ansL, ansL<<16); a stream that starts a
		// state below that was written by none.
		if x[j] < ansL {
			return x, 0, corrupt("ans state %d starts at %d, below %d", j, x[j], ansL)
		}
	}
	return x, 4 * ansStates, nil
}

// ansDecode0 decodes len(out) symbols of one table from the stream w. The
// four states advance in lockstep, four symbols per round, so their chains
// of multiplications overlap; a renormalisation reads at most one word.
func ansDecode0(out, w []byte, slots *ansSlots) error {
	x, at, err := ansStream(w)
	if err != nil {
		return err
	}
	x0, x1, x2, x3 := x[0], x[1], x[2], x[3]
	n := len(out)
	// Each round needs at most four words; the fast loop runs while four
	// remain, and the careful one finishes.
	i := 0
	for ; i+ansStates <= n && at+8 <= len(w); i += ansStates {
		e0 := slots[x0&(ansScale-1)]
		e1 := slots[x1&(ansScale-1)]
		e2 := slots[x2&(ansScale-1)]
		e3 := slots[x3&(ansScale-1)]
		o := out[i : i+4 : i+4]
		o[0], o[1], o[2], o[3] = byte(e0), byte(e1), byte(e2), byte(e3)
		x0 = (e0>>8&0xfff+1)*(x0>>ansScaleBits) + x0&(ansScale-1) - e0>>20
		x1 = (e1>>8&0xfff+1)*(x1>>ansScaleBits) + x1&(ansScale-1) - e1>>20
		x2 = (e2>>8&0xfff+1)*(x2>>ansScaleBits) + x2&(ansScale-1) - e2>>20
		x3 = (e3>>8&0xfff+1)*(x3>>ansScaleBits) + x3&(ansScale-1) - e3>>20
		if x0 < ansL {
			x0 = x0<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x1 < ansL {
			x1 = x1<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x2 < ansL {
			x2 = x2<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x3 < ansL {
			x3 = x3<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
	}
	states := [ansStates]*uint32{&x0, &x1, &x2, &x3}
	for ; i < n; i++ {
		st := states[i%ansStates]
		e := slots[*st&(ansScale-1)]
		out[i] = byte(e)
		*st = (e>>8&0xfff+1)*(*st>>ansScaleBits) + *st&(ansScale-1) - e>>20
		if *st < ansL {
			if at+2 > len(w) {
				return corrupt("ans stream ends early")
			}
			*st = *st<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
	}
	if x0 != ansL || x1 != ansL || x2 != ansL || x3 != ansL {
		return corrupt("ans stream does not end where its encoder began")
	}
	if at != len(w) {
		return corrupt("ans stream has %d bytes left", len(w)-at)
	}
	return nil
}

// ansDecodeCtx decodes len(out) symbols, symbol i with byExp[exp[i]]: exp
// holds each value's context source (a base value's exponent) and byExp
// maps it to its context's table. A value whose table is absent is
// ErrCorrupt. Four states advance in lockstep, as in ansDecode0.
func ansDecodeCtx(out, w []byte, byExp *[256]*ansSlots, exp []byte) error {
	x, at, err := ansStream(w)
	if err != nil {
		return err
	}
	x0, x1, x2, x3 := x[0], x[1], x[2], x[3]
	n := len(out)
	i := 0
	for ; i+ansStates <= n && at+8 <= len(w); i += ansStates {
		ex := exp[i : i+4 : i+4]
		t0, t1, t2, t3 := byExp[ex[0]], byExp[ex[1]], byExp[ex[2]], byExp[ex[3]]
		if t0 == nil || t1 == nil || t2 == nil || t3 == nil {
			return corrupt("ans context without a table at value %d", i)
		}
		e0 := t0[x0&(ansScale-1)]
		e1 := t1[x1&(ansScale-1)]
		e2 := t2[x2&(ansScale-1)]
		e3 := t3[x3&(ansScale-1)]
		o := out[i : i+4 : i+4]
		o[0], o[1], o[2], o[3] = byte(e0), byte(e1), byte(e2), byte(e3)
		x0 = (e0>>8&0xfff+1)*(x0>>ansScaleBits) + x0&(ansScale-1) - e0>>20
		x1 = (e1>>8&0xfff+1)*(x1>>ansScaleBits) + x1&(ansScale-1) - e1>>20
		x2 = (e2>>8&0xfff+1)*(x2>>ansScaleBits) + x2&(ansScale-1) - e2>>20
		x3 = (e3>>8&0xfff+1)*(x3>>ansScaleBits) + x3&(ansScale-1) - e3>>20
		if x0 < ansL {
			x0 = x0<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x1 < ansL {
			x1 = x1<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x2 < ansL {
			x2 = x2<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
		if x3 < ansL {
			x3 = x3<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
	}
	states := [ansStates]*uint32{&x0, &x1, &x2, &x3}
	for ; i < n; i++ {
		t := byExp[exp[i]]
		if t == nil {
			return corrupt("ans context without a table at value %d", i)
		}
		st := states[i%ansStates]
		e := t[*st&(ansScale-1)]
		out[i] = byte(e)
		*st = (e>>8&0xfff+1)*(*st>>ansScaleBits) + *st&(ansScale-1) - e>>20
		if *st < ansL {
			if at+2 > len(w) {
				return corrupt("ans stream ends early")
			}
			*st = *st<<16 | uint32(binary.LittleEndian.Uint16(w[at:]))
			at += 2
		}
	}
	if x0 != ansL || x1 != ansL || x2 != ansL || x3 != ansL {
		return corrupt("ans stream does not end where its encoder began")
	}
	if at != len(w) {
		return corrupt("ans stream has %d bytes left", len(w)-at)
	}
	return nil
}
