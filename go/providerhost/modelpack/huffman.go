package modelpack

import "sort"

// maxCodeLen bounds every code, so one 12-bit peek finds any symbol and the
// decoding table has 4,096 entries (8 KiB, inside a core's L1 cache).
const (
	maxCodeLen = 12
	tableSize  = 1 << maxCodeLen
	tableMask  = tableSize - 1
)

// codeLengths fills lens with Huffman code lengths of at most maxCodeLen for
// the symbols freq counts. At least two symbols must have a nonzero count.
// When the optimal code is deeper than maxCodeLen it halves the counts,
// keeping each nonzero count at least 1, and builds again; with 256 symbols
// of equal count the depth is 8, so the loop ends.
func codeLengths(freq *[256]uint64, lens *[256]uint8) {
	var f [256]uint64
	f = *freq
	for {
		if buildLengths(&f, lens) <= maxCodeLen {
			return
		}
		for i := range f {
			if f[i] > 0 {
				f[i] = (f[i] + 1) / 2
			}
		}
	}
}

// buildLengths is the two-queue Huffman construction over the nonzero
// counts of f; it returns the deepest length.
func buildLengths(f *[256]uint64, lens *[256]uint8) int {
	var syms []int
	for s, c := range f {
		lens[s] = 0
		if c > 0 {
			syms = append(syms, s)
		}
	}
	sort.Slice(syms, func(i, j int) bool {
		a, b := syms[i], syms[j]
		if f[a] != f[b] {
			return f[a] < f[b]
		}
		return a < b
	})
	m := len(syms)
	weight := make([]uint64, 2*m-1)
	parent := make([]int, 2*m-1)
	for i, s := range syms {
		weight[i] = f[s]
	}
	leaf, inner, next := 0, m, m
	take := func() int {
		if leaf < m && (inner >= next || weight[leaf] <= weight[inner]) {
			leaf++
			return leaf - 1
		}
		inner++
		return inner - 1
	}
	for next < 2*m-1 {
		a, b := take(), take()
		weight[next] = weight[a] + weight[b]
		parent[a], parent[b] = next, next
		next++
	}
	depth := make([]int, 2*m-1)
	deepest := 0
	for i := 2*m - 3; i >= 0; i-- {
		depth[i] = depth[parent[i]] + 1
		if i < m {
			lens[syms[i]] = uint8(depth[i])
			deepest = max(deepest, depth[i])
		}
	}
	return deepest
}

// canonicalCodes gives each symbol with a nonzero length its canonical code
// (RFC 1951 §3.2.2), bit-reversed for least-significant-bit-first output.
func canonicalCodes(lens *[256]uint8, codes *[256]uint16) {
	var count [maxCodeLen + 1]uint16
	for _, l := range lens {
		count[l]++
	}
	count[0] = 0
	var next [maxCodeLen + 2]uint16
	code := uint16(0)
	for l := 1; l <= maxCodeLen; l++ {
		code = (code + count[l-1]) << 1
		next[l] = code
	}
	for s, l := range lens {
		if l == 0 {
			codes[s] = 0
			continue
		}
		c := next[l]
		next[l]++
		codes[s] = reverse(c, l)
	}
}

func reverse(c uint16, n uint8) uint16 {
	r := uint16(0)
	for range n {
		r = r<<1 | c&1
		c >>= 1
	}
	return r
}

// decodeTable is indexed by the next 12 bits of a substream, least
// significant first. Each entry holds the symbol in its low byte and the
// code length in its high byte.
type decodeTable [tableSize]uint16

// buildTable accepts only a complete prefix code: at least two symbols, each
// length at most maxCodeLen, and a Kraft sum of exactly one, so every 12-bit
// index names one symbol.
func buildTable(lens *[256]uint8, tab *decodeTable) bool {
	used, kraft := 0, 0
	for _, l := range lens {
		if l > maxCodeLen {
			return false
		}
		if l > 0 {
			used++
			kraft += tableSize >> l
		}
	}
	if used < 2 || kraft != tableSize {
		return false
	}
	var codes [256]uint16
	canonicalCodes(lens, &codes)
	for s, l := range lens {
		if l == 0 {
			continue
		}
		e := uint16(s) | uint16(l)<<8
		for i := int(codes[s]); i < tableSize; i += 1 << l {
			tab[i] = e
		}
	}
	return true
}
