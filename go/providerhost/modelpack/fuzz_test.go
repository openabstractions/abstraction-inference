package modelpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// FuzzDecodeChunk feeds the decoder arbitrary payloads, up to a full
// 512 KiB unit. It must refuse or decode without panicking or writing
// outside its slot, and a payload it accepts must decode to the same bytes
// when decoders start mid-chunk. The seed corpus is in
// testdata/fuzz/FuzzDecodeChunk.
func FuzzDecodeChunk(f *testing.F) {
	for _, s := range seedPayloads() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) < headerLen {
			if err := DecodeChunk(nil, payload); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("short payload: %v", err)
			}
			return
		}
		n := binary.LittleEndian.Uint32(payload[4:])
		if n == 0 || n > ChunkBytes/2 {
			n = 64
		}
		// Guard bytes on both sides of the slot catch an out-of-slot write.
		buf := make([]byte, 2*n+64)
		dst := buf[32 : 32+2*n : 32+2*n]
		err := DecodeChunk(dst, payload)
		for i, b := range append(buf[:32:32], buf[32+2*n:]...) {
			if b != 0 {
				t.Fatalf("guard byte %d written", i)
			}
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("refusal %v is not ErrCorrupt", err)
			}
			return
		}
		var c chunk
		defer c.release()
		if err := c.parse(payload); err != nil {
			t.Fatalf("parse refuses what DecodeChunk accepted: %v", err)
		}
		split := make([]byte, len(dst))
		for _, r := range [][2]int{{0, 13}, {13, 128}, {128, 256}} {
			if err := c.decode(split, r[0], r[1]); err != nil {
				t.Fatalf("substreams %v: %v", r, err)
			}
		}
		if !bytes.Equal(split, dst) {
			t.Fatal("decoding from mid-chunk gives other bytes")
		}
	})
}

// fullUnit is a whole 512 KiB BF16 unit whose payload stays small: two
// exponents and two mantissas, so both planes are coded at about one bit a
// value and the unit decodes through the lane path.
func fullUnit() []byte {
	v := make([]uint16, ChunkBytes/2)
	x := uint32(1)
	for i := range v {
		x = x*1664525 + 1013904223
		v[i] = uint16(120+x>>31)<<7 | uint16(0x15<<(x>>30&1))
	}
	return values16(v...)
}

// seedPayloads are valid payloads of every plane mode pair, a full unit and
// small tails, and a few malformed ones.
func seedPayloads() [][]byte {
	var out [][]byte
	add := func(src []byte, dtype string) {
		p, err := EncodeChunk(nil, src, dtype)
		if err != nil {
			panic(err)
		}
		out = append(out, p)
	}
	add(weights(1100, 1, bf16), "BF16")          // coded high, raw low
	add(weights(1027, 2, f16FromBF16), "F16")    // both coded
	add(values16(0x3c00, 0x3c00, 0x3c00), "F16") // both constant
	add(randomBytes(2*301, 3), "BF16")           // both raw
	add(weights(5, 4, bf16), "BF16")
	add(values16(0x7f80, 0xff80, 0x7fc0, 0x0001, 0x8001), "BF16")
	same := make([]uint16, 2000) // one exponent: constant high plane
	for i := range same {
		same[i] = uint16(i%2)<<15 | 120<<7 | uint16(i%128)
	}
	add(values16(same...), "BF16")
	out = append(out, nil, make([]byte, headerLen), []byte{1, 1, 1, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	add(fullUnit(), "BF16") // last, so the earlier seed file names keep their payloads
	return out
}

// FuzzDecodeExpFixed3 feeds the fixed-code decoder arbitrary payloads, up
// to a full unit. It must refuse with ErrCorrupt or decode without
// panicking or writing outside its slot; a payload it accepts must
// re-encode, when its values fit the code, to a payload that decodes to the
// same bytes, and the fused decode with its escape pass must give the same
// bytes. The seed corpus is in testdata/fuzz/FuzzDecodeExpFixed3.
func FuzzDecodeExpFixed3(f *testing.F) {
	for _, s := range fixedSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		n := 64
		if len(payload) >= 8 {
			if c := int(binary.LittleEndian.Uint32(payload[4:])); c > 0 && c <= maxChunkValues {
				n = c
			}
		}
		buf := make([]byte, 2*n+64)
		dst := buf[32 : 32+2*n : 32+2*n]
		err := DecodeExpFixed3(dst, payload)
		for i, b := range append(buf[:32:32], buf[32+2*n:]...) {
			if b != 0 {
				t.Fatalf("guard byte %d written", i)
			}
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("refusal %v is not ErrCorrupt", err)
			}
			return
		}
		// The fused decode, its escapes applied afterwards, gives the same
		// bytes.
		fused := make([]byte, len(dst))
		d := newFixedPatch(&ChunkEntry{Length: int64(len(dst))})
		if err := decodeFixed(fused, payload, d); err != nil {
			t.Fatalf("the fused decode refuses what DecodeExpFixed3 accepts: %v", err)
		}
		if k := d.apply(fused, 0, 0, n, 0); k != len(d.escapes) || !bytes.Equal(fused, dst) {
			t.Fatalf("the fused decode used %d of %d escapes and gives other bytes", k, len(d.escapes))
		}
		// The device's check finds the same escape positions.
		s := newFixedPatch(&ChunkEntry{Length: int64(len(dst))})
		if _, err := scanFixed(payload, len(dst), s); err != nil || !slices.Equal(s.offsets, d.offsets) || !slices.Equal(s.starts, d.starts) {
			t.Fatalf("scanFixed: %v; its positions differ from the decode's", err)
		}
		again, err := EncodeExpFixed3(nil, dst, "BF16")
		if err != nil {
			return // the decoded values need more escapes than the encoder writes
		}
		back := make([]byte, len(dst))
		if err := DecodeExpFixed3(back, again); err != nil || !bytes.Equal(back, dst) {
			t.Fatalf("re-encoded payload: %v", err)
		}
	})
}

// fixedSeeds are valid fixed-code payloads of one and two chunks, a tail,
// specials, and malformed ones.
func fixedSeeds() [][]byte {
	var out [][]byte
	add := func(src []byte) {
		p, err := EncodeExpFixed3(nil, src, "BF16")
		if err != nil {
			panic(err)
		}
		out = append(out, p)
	}
	add(weights(1000, 80, bf16))
	add(weights(4096+9, 81, bf16))
	add(weights(5, 82, bf16))
	add(values16(append(slices.Repeat([]uint16{0x3c00}, 200), 0x7f80, 0xff80, 0x7fc0, 0x0001, 0x8001)...))
	out = append(out, nil, make([]byte, fixedHeaderLen), []byte{1, 1, 0, 0, 8, 0, 0, 0, 1, 0, 0, 0})
	return out
}

// fuzzPack is one real pack the index fuzz target mutates: a stored header,
// three BF16 units, the last one short, and a stored trailer.
var fuzzPack = sync.OnceValues(func() (Index, []byte) {
	src := slices.Concat(randomBytes(300, 50), weights(ChunkBytes+ChunkBytes/4, 51, bf16), randomBytes(41, 52))
	tensors := []Tensor{{Offset: 300, Length: int64(len(src) - 341), DType: "BF16"}}
	var b bytes.Buffer
	index, err := Pack(&b, src, tensors, fast)
	if err != nil {
		panic(err)
	}
	return index, b.Bytes()
})

// FuzzUnpackIndex mutates one field of one unit of a real pack's index, or
// the index's length, and unpacks it against the recorded spans. Unpack
// must refuse with ErrCorrupt or modelfiles.ErrDigest, or give exactly the
// object's bytes, and never panic. The seed corpus is in
// testdata/fuzz/FuzzUnpackIndex.
func FuzzUnpackIndex(f *testing.F) {
	for _, s := range indexSeeds() {
		f.Add(s.unit, s.field, s.value, s.encoding)
	}
	f.Fuzz(func(t *testing.T, unit, field uint8, value int64, encoding string) {
		index, packed := fuzzPack()
		want := make([]byte, index.Length)
		if err := Unpack(context.Background(), want, bytes.NewReader(packed), index, index.Spans, 2); err != nil {
			t.Fatalf("the unmutated pack: %v", err)
		}
		bad := index
		bad.Chunks = slices.Clone(index.Chunks)
		c := &bad.Chunks[int(unit)%len(bad.Chunks)]
		switch field % 6 {
		case 0:
			c.Offset = value
		case 1:
			c.Length = value
		case 2:
			c.PackedOffset = value
		case 3:
			c.Packed = value
		case 4:
			c.Encoding = Encoding(encoding)
		case 5:
			bad.Length = value
		}
		// The buffer is the real object's size, as a caller's section is.
		dst := make([]byte, index.Length)
		err := Unpack(context.Background(), dst, bytes.NewReader(packed), bad, index.Spans, 3)
		switch {
		case err == nil:
			if !bytes.Equal(dst, want) {
				t.Fatal("a mutated index was accepted with other bytes")
			}
		case !errors.Is(err, ErrCorrupt) && !errors.Is(err, modelfiles.ErrDigest):
			t.Fatalf("refusal %v is neither ErrCorrupt nor modelfiles.ErrDigest", err)
		}
	})
}

type indexSeed struct {
	unit, field uint8
	value       int64
	encoding    string
}

func indexSeeds() []indexSeed {
	return []indexSeed{
		{0, 0, 1, ""},
		{1, 1, ChunkBytes - 2, ""},
		{1, 1, 1 << 40, ""},
		{2, 2, 0, ""},
		{2, 2, -1, ""},
		{3, 3, 1, ""},
		{3, 3, 1 << 62, ""},
		{1, 4, 0, "stored"},
		{0, 4, 0, "exp-huffman-v1"},
		{4, 4, 0, "zstd"},
		{1, 4, 0, "exp-fixed3-v1"},
		{0, 5, 0, ""},
		{0, 5, -1, ""},
		{1, 0, -1 << 63, ""},
	}
}

// TestWriteFuzzSeeds rewrites the checked-in seed corpora from seedPayloads
// and indexSeeds when OA_MODELPACK_WRITE_SEEDS=1; otherwise it checks both
// corpora exist.
func TestWriteFuzzSeeds(t *testing.T) {
	chunks := filepath.Join("testdata", "fuzz", "FuzzDecodeChunk")
	indexes := filepath.Join("testdata", "fuzz", "FuzzUnpackIndex")
	fixed := filepath.Join("testdata", "fuzz", "FuzzDecodeExpFixed3")
	if os.Getenv("OA_MODELPACK_WRITE_SEEDS") != "1" {
		for _, dir := range []string{chunks, indexes, fixed} {
			if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
				t.Fatalf("seed corpus %s is missing: %v", dir, err)
			}
		}
		return
	}
	write := func(dir string, i int, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("seed-%02d", i)), []byte("go test fuzz v1\n"+body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range seedPayloads() {
		write(chunks, i, fmt.Sprintf("[]byte(%q)\n", p))
	}
	for i, p := range fixedSeeds() {
		write(fixed, i, fmt.Sprintf("[]byte(%q)\n", p))
	}
	for i, s := range indexSeeds() {
		write(indexes, i, fmt.Sprintf("uint8(%d)\nuint8(%d)\nint64(%d)\nstring(%q)\n", s.unit, s.field, s.value, s.encoding))
	}
}
