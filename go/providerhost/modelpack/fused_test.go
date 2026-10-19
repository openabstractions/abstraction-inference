package modelpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"sync"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// fixedOnly packs BF16 with the fixed code alone.
var fixedOnly = Rates{BF16Fixed3: 20e9, Disk: 6.5e9}

// fusedObject is a BF16 tensor after a header of odd length, so its values
// cross the 16 MiB span boundary with one value split across it. NaNs,
// escapes in any window, sit every 997 values, on the split value, and on
// values split across 256 KiB hash blocks.
func fusedObject() ([]byte, []Tensor) {
	header := randomBytes(1001, 80)
	bf := weights(9<<20, 81, bf16) // 18 MiB
	src := slices.Concat(header, bf, randomBytes(33, 82))
	at := int64(len(header))
	nan := func(off int64) { // the value holding byte off
		v := (off - at) / 2
		binary.LittleEndian.PutUint16(src[at+2*v:], 0x7fc1)
	}
	for v := int64(0); v < int64(len(bf))/2; v += 997 {
		nan(at + 2*v)
	}
	nan(modelfiles.ChunkSize - 1)
	for k := int64(1); k < 8; k++ {
		nan(k*hashBlockForTest - 1)
	}
	return src, []Tensor{{Offset: at, Length: int64(len(bf)), DType: "BF16"}}
}

// hashBlockForTest is modelfiles' hash block, 256 KiB.
const hashBlockForTest = 256 << 10

// INF-MP7, INF-MP8: a fused decode leaves each fixed-code escape for the
// hash pass, which applies them block by block as a fill hashes each span on
// its own goroutine. The bytes and spans equal the object's at every block
// size, values split across blocks and the split value across spans
// included.
func TestFusedEscapesMatchTheUnfusedDecode(t *testing.T) {
	src, tensors := fusedObject()
	index, packed := pack(t, src, tensors, fixedOnly)
	if encodings(index)[EncodingExpFixed3V1] == 0 {
		t.Fatal("no unit took the fixed code")
	}
	read := readerAt(bytes.NewReader(packed))
	open := func() (readFunc, func(), error) { return read, func() {}, nil }
	for _, block := range []int{hashBlockForTest, 4097, 2, modelfiles.ChunkSize} {
		dst := make([]byte, len(src))
		patches := make([]*fixedPatch, len(index.Chunks))
		if err := unpack(context.Background(), dst, index, index.Spans, 4, false, open, patches, nil, nil); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(dst, src) {
			t.Fatal("the fused decode applied every escape itself")
		}
		split := slices.IndexFunc(patches, func(p *fixedPatch) bool { return p != nil && p.inline >= 0 })
		if split < 0 {
			t.Fatal("no unit has a value split across the spans")
		}
		finish := escapePass(dst, index.Chunks, patches)
		var wg sync.WaitGroup
		for s := range index.Spans {
			wg.Go(func() {
				span := spanOf(dst, s)
				complete := finish(s)
				for at := 0; at < len(span); at += block {
					complete(span[at:min(at+block, len(span))])
				}
			})
		}
		wg.Wait()
		if !bytes.Equal(dst, src) {
			i := 0
			for dst[i] == src[i] {
				i++
			}
			t.Fatalf("blocks of %d: byte %d differs after the escape pass", block, i)
		}
		if !slices.Equal(spanDigests(dst), index.Spans) {
			t.Fatalf("blocks of %d: spans differ", block)
		}
	}
	// Unpack and Restore apply escapes themselves.
	if got, err := unpackAll(t, packed, index, len(src), 4); err != nil || !bytes.Equal(got, src) {
		t.Fatalf("unfused Unpack: %v", err)
	}
}

// INF-MP8: each chunk header holds the count of the escapes the unit's
// earlier chunks use, and a decoder refuses any other count.
func TestFixedChunkEscapeOffsets(t *testing.T) {
	src := weights(5*fixedChunkVals+17, 83, bf16)
	for i := 0; i < len(src); i += 2 * 331 {
		binary.LittleEndian.PutUint16(src[i:], 0x7fc1)
	}
	p := fixedRoundTrip(t, src)
	escapes := 0
	for c := range fixedChunks(len(src) / 2) {
		chunk := p[fixedHeaderLen+c*fixedChunkLen:]
		if got := int(binary.LittleEndian.Uint32(chunk[fixedEscapeAt:])); got != escapes {
			t.Fatalf("chunk %d starts at escape %d, want %d", c, got, escapes)
		}
		for v := c * fixedChunkVals; v < min((c+1)*fixedChunkVals, len(src)/2); v++ {
			if exp := int(binary.LittleEndian.Uint16(src[2*v:])>>7) & 0xff; exp < int(chunk[0]) || exp > int(chunk[0])+6 {
				escapes++
			}
		}
	}
	if escapes != int(binary.LittleEndian.Uint32(p[8:])) || escapes == 0 {
		t.Fatalf("%d escapes counted, the header says %d", escapes, binary.LittleEndian.Uint32(p[8:]))
	}
}
