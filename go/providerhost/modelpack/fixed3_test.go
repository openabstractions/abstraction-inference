package modelpack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

func fixedRoundTrip(t *testing.T, src []byte) []byte {
	t.Helper()
	p, err := EncodeExpFixed3(nil, src, "BF16")
	if err != nil {
		t.Fatalf("EncodeExpFixed3: %v", err)
	}
	dst := bytes.Repeat([]byte{0xa5}, len(src))
	if err := DecodeExpFixed3(dst, p); err != nil {
		t.Fatalf("DecodeExpFixed3: %v", err)
	}
	if !bytes.Equal(dst, src) {
		i := 0
		for dst[i] == src[i] {
			i++
		}
		t.Fatalf("decoded bytes differ from byte %d of %d (value %d)", i, len(src), i/2)
	}
	return p
}

// INF-MP8: BF16 values return bit for bit, escapes included (NaN, Inf,
// subnormals, zero beside a window that excludes it), at every tail length
// around the 8-value groups and 4,096-value chunks.
func TestFixedRoundTripBitExact(t *testing.T) {
	special := []uint16{0x0000, 0x8000, 0x7f80, 0xff80, 0x7fc0, 0xffc1, 0x7f81, 0x0001, 0x8001, 0x007f, 0x7f7f, 0xff7f}
	mixed := weights(ChunkBytes/2, 60, bf16)
	for i, v := range special {
		binary.LittleEndian.PutUint16(mixed[2*(i*997):], v)
	}
	zeros := weights(20000, 61, bf16)
	for i := 0; i < len(zeros); i += 2 { // 15 values in 16 are zero: window base 0
		if i/2%16 != 0 {
			zeros[i], zeros[i+1] = 0, 0
		}
	}
	for name, src := range map[string][]byte{
		"weights, full unit":        weights(ChunkBytes/2, 62, bf16),
		"special values":            mixed,
		"many zeros":                zeros,
		"one value repeated":        values16(slices.Repeat([]uint16{0x3c80}, 5000)...),
		"largest unit":              weights(maxChunkValues, 63, bf16),
		"specials alone, few":       values16(append(slices.Repeat([]uint16{0x3c00}, 100), special...)...),
		"top of the exponent range": values16(slices.Repeat([]uint16{0x7f00, 0x7f7f, 0x7e80, 0xff00}, 300)...),
	} {
		t.Run(name, func(t *testing.T) { fixedRoundTrip(t, src) })
	}
	for _, n := range []int{1, 2, 7, 8, 9, 63, 64, 65, 511, 512, 4095, 4096, 4097, 8191, 2*4096 + 13} {
		fixedRoundTrip(t, weights(n, uint64(n)+70, bf16))
	}
}

// INF-MP8: the fixed code keeps about 0.70 of normal-shaped weights, and
// refuses a unit with more than one value in eight outside its windows and
// any input that is not BF16.
func TestFixedRatioAndRefusals(t *testing.T) {
	src := weights(ChunkBytes/2, 64, bf16)
	p := fixedRoundTrip(t, src)
	if kept := float64(len(p)) / float64(len(src)); kept < 0.69 || kept > 0.73 {
		t.Fatalf("kept %.4f of normal-shaped BF16", kept)
	}
	if _, err := EncodeExpFixed3(nil, randomBytes(ChunkBytes, 65), "BF16"); !errors.Is(err, errEscapes) {
		t.Fatalf("random bits: %v, want the escape refusal", err)
	}
	for _, c := range []struct {
		dtype string
		src   []byte
	}{{"F16", src}, {"Q4_K", src}, {"BF16", src[:3]}, {"BF16", nil}, {"BF16", make([]byte, 2*maxChunkValues+2)}} {
		if _, err := EncodeExpFixed3(nil, c.src, c.dtype); !errors.Is(err, ErrDType) {
			t.Errorf("%s of %d bytes: %v, want ErrDType", c.dtype, len(c.src), err)
		}
	}
}

// INF-MP8: a payload that contradicts its layout is refused.
func TestFixedDecodeRefusals(t *testing.T) {
	src := weights(5000, 66, bf16)
	p, err := EncodeExpFixed3(nil, src, "BF16")
	if err != nil {
		t.Fatal(err)
	}
	nesc := int(binary.LittleEndian.Uint32(p[8:]))
	if nesc == 0 {
		t.Fatal("the sample has no escapes; the escape cases test nothing")
	}
	dst := make([]byte, len(src))
	edit := func(f func(q []byte) []byte) []byte { return f(bytes.Clone(p)) }
	for name, q := range map[string][]byte{
		"cut short":         p[:len(p)-1],
		"a byte added":      append(bytes.Clone(p), 0),
		"version":           edit(func(q []byte) []byte { q[0] = 2; return q }),
		"dtype":             edit(func(q []byte) []byte { q[1] = dtypeF16; return q }),
		"header byte":       edit(func(q []byte) []byte { q[40] = 1; return q }),
		"base above 248":    edit(func(q []byte) []byte { q[fixedHeaderLen] = 249; return q }),
		"chunk header byte": edit(func(q []byte) []byte { q[fixedHeaderLen+3] = 1; return q }),
		"chunk header byte after the escape index": edit(func(q []byte) []byte { q[fixedHeaderLen+fixedEscapeAt+4] = 1; return q }),
		"first chunk's escape index":               edit(func(q []byte) []byte { q[fixedHeaderLen+fixedEscapeAt] = 1; return q }),
		"second chunk's escape index": edit(func(q []byte) []byte {
			at := fixedHeaderLen + fixedChunkLen + fixedEscapeAt
			binary.LittleEndian.PutUint32(q[at:], binary.LittleEndian.Uint32(q[at:])+1)
			return q
		}),
		"one escape too few": edit(func(q []byte) []byte { binary.LittleEndian.PutUint32(q[8:], uint32(nesc-1)); return q[:len(q)-1] }),
		"one escape extra":   edit(func(q []byte) []byte { binary.LittleEndian.PutUint32(q[8:], uint32(nesc+1)); return append(q, 120) }),
		"escapes over 1/8":   edit(func(q []byte) []byte { binary.LittleEndian.PutUint32(q[8:], 5000/8+1); return q }),
		"no values":          edit(func(q []byte) []byte { binary.LittleEndian.PutUint32(q[4:], 0); return q }),
	} {
		if err := DecodeExpFixed3(dst, q); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
		}
	}
	if err := DecodeExpFixed3(dst[:len(dst)-2], p); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a slot one value short: %v", err)
	}
}

// INF-MP2: Pack picks per unit by the shorter load. With exp-huffman-v1
// decoding slower than the disk can feed it the fixed code wins; with
// headroom exp-huffman-v1 wins; a unit the fixed code refuses takes
// exp-huffman-v1 or stays stored; F16 never takes the fixed code.
func TestPackChoosesBetweenEncodings(t *testing.T) {
	src := slices.Concat(weights(ChunkBytes/2, 67, bf16), randomBytes(ChunkBytes, 68))
	tensors := whole(src, "BF16")
	cases := []struct {
		name  string
		rates Rates
		want  [2]Encoding
	}{
		{"exp-huffman-v1 decode-bound", Rates{BF16: 7e9, BF16Fixed3: 20e9, Disk: 6.5e9}, [2]Encoding{EncodingExpFixed3V1, EncodingStored}},
		{"exp-huffman-v1 with headroom", Rates{BF16: 30e9, BF16Fixed3: 30e9, Disk: 6.5e9}, [2]Encoding{EncodingExpHuffmanV1, EncodingStored}},
		{"fixed code only", Rates{BF16Fixed3: 20e9, Disk: 6.5e9}, [2]Encoding{EncodingExpFixed3V1, EncodingStored}},
		{"fixed code slower than the disk", Rates{BF16: 7e9, BF16Fixed3: 6e9, Disk: 6.5e9}, [2]Encoding{EncodingExpHuffmanV1, EncodingStored}},
	}
	for _, c := range cases {
		index, packed := pack(t, src, tensors, c.rates)
		if got := [2]Encoding{index.Chunks[0].Encoding, index.Chunks[1].Encoding}; got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if got, err := unpackAll(t, packed, index, len(src), 4); err != nil || !bytes.Equal(got, src) {
			t.Fatalf("%s: unpack: %v", c.name, err)
		}
	}
	// One value in five is a NaN: too many escapes for the fixed code;
	// exp-huffman-v1 still saves.
	noisy := weights(ChunkBytes/2, 69, bf16)
	for i := 0; i < len(noisy); i += 10 {
		binary.LittleEndian.PutUint16(noisy[i:], 0x7fc1)
	}
	if index, _ := pack(t, noisy, whole(noisy, "BF16"), Rates{BF16: 7e9, BF16Fixed3: 20e9, Disk: 6.5e9}); index.Chunks[0].Encoding != EncodingExpHuffmanV1 {
		t.Errorf("a unit the fixed code refuses: %s", index.Chunks[0].Encoding)
	}
	f16s := weights(ChunkBytes/2, 70, f16)
	if index, _ := pack(t, f16s, whole(f16s, "F16"), Rates{F16: 7e9, BF16Fixed3: 20e9, Disk: 6.5e9}); index.Chunks[0].Encoding != EncodingExpHuffmanV1 {
		t.Errorf("F16: %s", index.Chunks[0].Encoding)
	}
}

// INF-MP2: Pack decodes a fixed-code unit before writing it; a broken
// fixed encoder refuses the pack.
func TestPackRefusesBrokenFixedEncoder(t *testing.T) {
	src := weights(ChunkBytes/2, 71, bf16)
	defer func(e func([]byte, []byte, string) ([]byte, error)) { encodeFixed = e }(encodeFixed)
	encodeFixed = func(dst, b []byte, dtype string) ([]byte, error) {
		p, err := EncodeExpFixed3(dst, b, dtype)
		if err == nil {
			p[len(p)-1] ^= 1 // an escape's exponent
		}
		return p, err
	}
	var w bytes.Buffer
	if _, err := Pack(&w, src, whole(src, "BF16"), Rates{BF16Fixed3: 20e9, Disk: 6.5e9}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("broken fixed encoder: %v, want ErrCorrupt", err)
	}
}

// INF-MP1: an index entry of the fixed code is bounded by its layout.
func TestFixedIndexBounds(t *testing.T) {
	src := weights(ChunkBytes/2, 72, bf16)
	index, packed := pack(t, src, whole(src, "BF16"), Rates{BF16Fixed3: 20e9, Disk: 6.5e9})
	if index.Chunks[0].Encoding != EncodingExpFixed3V1 {
		t.Fatalf("encoding %s", index.Chunks[0].Encoding)
	}
	for name, edit := range map[string]func(c *ChunkEntry){
		"packed past the bound": func(c *ChunkEntry) { c.Packed = maxPackedFixed(c.Length) + 1 },
		"odd length":            func(c *ChunkEntry) { c.Length-- },
	} {
		bad := index
		bad.Chunks = slices.Clone(index.Chunks)
		edit(&bad.Chunks[0])
		dst := make([]byte, len(src))
		if err := Unpack(context.Background(), dst, bytes.NewReader(packed), bad, index.Spans, 2); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
		}
	}
}

// BenchmarkDecodeExpFixed3 decodes one 512 KiB unit on one goroutine.
func BenchmarkDecodeExpFixed3(b *testing.B) {
	src := weights(ChunkBytes/2, 1, bf16)
	p, err := EncodeExpFixed3(nil, src, "BF16")
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]byte, len(src))
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		if err := DecodeExpFixed3(dst, p); err != nil {
			b.Fatal(err)
		}
	}
}
