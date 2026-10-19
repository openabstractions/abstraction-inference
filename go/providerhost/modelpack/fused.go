package modelpack

import (
	"encoding/binary"
	"slices"
	"sort"
	"sync/atomic"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// escapePass is a fused fill's finish (modelfiles.Decoder): view is the
// object as unpack left it with patches, and finish(span) returns the
// function that applies the escapes of the exp-fixed3-v1 units in that span
// to each block the fill hands it, just before the fill hashes the block.
// A block's last value may end in the span's next block, which the
// function patches ahead; the value that ends in the next span was applied
// by decodeFixed. It is nil when no unit left an escape. A unit's patch is
// dropped from patches once every span holding its bytes has been passed,
// so the fill holds escapes only for spans still to hash.
func escapePass(view []byte, units []ChunkEntry, patches []*fixedPatch) func(span int) func(block []byte) {
	if !slices.ContainsFunc(patches, func(p *fixedPatch) bool { return p != nil }) {
		return nil
	}
	// refs[u] counts the spans whose pass has not finished unit u.
	refs := make([]atomic.Int32, len(units))
	for i := range units {
		first, last := spanRange(&units[i])
		refs[i].Store(int32(last - first + 1))
	}
	done := func(u int) {
		if refs[u].Add(-1) == 0 {
			patches[u] = nil
		}
	}
	return func(span int) func([]byte) {
		start := int64(span) * modelfiles.ChunkSize
		spanEnd := min(start+modelfiles.ChunkSize, int64(len(view)))
		u := sort.Search(len(units), func(i int) bool { return units[i].Offset+units[i].Length > start })
		pos := start
		v, k := -1, 0 // the next value of unit u to patch and its escape index; -1 before the unit
		return func(block []byte) {
			end := min(pos+int64(len(block)), spanEnd)
			for u < len(units) && units[u].Offset < end {
				c, d := &units[u], patches[u]
				if d != nil {
					if v < 0 {
						v, k = 0, 0
						if c.Offset < start {
							v, k = d.split, d.before
						}
					}
					hi := int(min(c.Length/2, (end-c.Offset+1)/2))
					k = d.apply(view, c.Offset, v, hi, k)
					v = hi
				}
				if c.Offset+c.Length > end {
					if end == spanEnd { // the span's part of the unit is passed
						done(u)
					}
					break
				}
				done(u)
				u, v = u+1, -1
			}
			pos = end
		}
	}
}

// apply patches the escaped values among [v, hi) of the unit at off in
// view, whose escape index at v is k, and returns the escape index at hi.
// It visits only the positions the decode recorded (d.offsets, d.starts),
// so its cost follows the escapes. It writes a value only where it lies in
// the unit and carries its chunk's mark b+7. A wrong or altered position
// list therefore leaves an escape at its mark or a value unpatched, and the
// fill's hash refuses the bytes; it never reads or writes outside the
// unit's values below hi.
func (d *fixedPatch) apply(view []byte, off int64, v, hi, k int) int {
	n := min(len(d.offsets), len(d.escapes))
	if k >= n {
		return k
	}
	// c is the chunk of escape k: the last chunk whose first escape is at
	// or before k.
	c := sort.Search(len(d.starts), func(i int) bool { return d.starts[i] > uint32(k) }) - 1
	for ; k < n; k++ {
		for c+1 < len(d.starts) && d.starts[c+1] <= uint32(k) {
			c++
		}
		if c < 0 || c >= len(d.bases) {
			break
		}
		p := c*fixedChunkVals + int(d.offsets[k])
		if p >= hi {
			break
		}
		if p < v || p == d.inline {
			continue
		}
		o := off + 2*int64(p)
		x := binary.LittleEndian.Uint16(view[o:])
		if x>>7&0xff == uint16(d.bases[c])+7 {
			binary.LittleEndian.PutUint16(view[o:], x&0x807f|uint16(d.escapes[k])<<7)
		}
	}
	return k
}
