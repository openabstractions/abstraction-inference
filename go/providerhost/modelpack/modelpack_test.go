package modelpack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// fast is a machine that decodes both dtypes faster than its disk reads.
var fast = Rates{BF16: 15e9, F16: 15e9, Disk: 6.5e9}

func pack(t *testing.T, src []byte, tensors []Tensor, r Rates) (Index, []byte) {
	t.Helper()
	var b bytes.Buffer
	index, err := Pack(&b, src, tensors, r)
	if err != nil {
		t.Fatalf("Pack: %v", err)
	}
	return index, b.Bytes()
}

func whole(src []byte, dtype string) []Tensor {
	return []Tensor{{Offset: 0, Length: int64(len(src)), DType: dtype}}
}

func unpackAll(t *testing.T, packed []byte, index Index, n, workers int) ([]byte, error) {
	t.Helper()
	dst := make([]byte, n)
	return dst, Unpack(context.Background(), dst, bytes.NewReader(packed), index, index.Spans, workers)
}

func encodings(index Index) map[Encoding]int {
	m := map[Encoding]int{}
	for _, c := range index.Chunks {
		m[c.Encoding]++
	}
	return m
}

// spanSums is the proof modelfiles records for a filled object: the hex
// SHA-256 of each modelfiles.ChunkSize span.
func spanSums(b []byte) []string {
	var out []string
	for off := 0; off < len(b); off += modelfiles.ChunkSize {
		sum := sha256.Sum256(b[off:min(off+modelfiles.ChunkSize, len(b))])
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

// object is a 20 MiB model file: a header, a BF16 tensor that crosses the
// 16 MiB span boundary at an odd offset, a quantised tensor, an F16 tensor
// and a trailer.
func object() ([]byte, []Tensor) {
	header := randomBytes(1000, 40)
	bf := weights(9<<20, 41, bf16) // 18 MiB
	q4 := randomBytes(1<<20, 42)
	f := weights(300_001, 43, f16)
	trailer := randomBytes(77, 44)
	src := slices.Concat(header, bf, q4, f, trailer)
	at := int64(len(header))
	tensors := []Tensor{
		{Offset: at, Length: int64(len(bf)), DType: "BF16"},
		{Offset: at + int64(len(bf)), Length: int64(len(q4)), DType: "Q4_K"},
		{Offset: at + int64(len(bf)+len(q4)), Length: int64(len(f)), DType: "F16"},
	}
	return src, tensors
}

// INF-MP1: the index proves the object with the span digests memory lending
// records (INF-MS2), and units tile the object in order.
func TestIndexSpansAreTheSectionProof(t *testing.T) {
	src, tensors := object()
	index, _ := pack(t, src, tensors, fast)
	if !slices.Equal(index.Spans, spanSums(src)) || len(index.Spans) != 2 {
		t.Fatalf("spans %v, want the SHA-256 of each 16 MiB span", index.Spans)
	}
	var at int64
	for _, c := range index.Chunks {
		if c.Offset != at {
			t.Fatalf("unit at %d, want %d", c.Offset, at)
		}
		at += c.Length
	}
	if at != int64(len(src)) || index.Length != at {
		t.Fatalf("units cover %d of %d bytes", at, len(src))
	}
}

// INF-MP1, INF-MP4: a packed object unpacks to its bytes on one goroutine
// and on many, and the parallel result equals the serial one, with units
// that cross a span boundary.
func TestUnpackParallelEqualsSerial(t *testing.T) {
	src, tensors := object()
	index, packed := pack(t, src, tensors, fast)
	crosses := false
	for i := range index.Chunks {
		if a, b := spanRange(&index.Chunks[i]); a != b {
			crosses = true
		}
	}
	if !crosses {
		t.Fatal("no unit crosses a span boundary; the test object lost its point")
	}
	serial, err := unpackAll(t, packed, index, len(src), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{2, 8, 32, 0} {
		parallel, err := unpackAll(t, packed, index, len(src), w)
		if err != nil {
			t.Fatalf("%d workers: %v", w, err)
		}
		if !bytes.Equal(parallel, serial) {
			t.Fatalf("%d workers give other bytes than one", w)
		}
	}
	if !bytes.Equal(serial, src) {
		t.Fatal("unpacked bytes differ from the object")
	}
}

// INF-MP2: units are encoded only inside BF16 and F16 tensors, only where
// they save 3 percent, and only when the machine decodes that dtype faster
// than its disk reads; everything else is stored in runs of at most 16 MiB.
func TestPackChoosesPerUnit(t *testing.T) {
	src, tensors := object()
	bfEnd := tensors[0].Offset + tensors[0].Length
	check := func(name string, r Rates, wantF16 Encoding) {
		index, packed := pack(t, src, tensors, r)
		for _, c := range index.Chunks {
			inBF := c.Offset >= tensors[0].Offset && c.Offset < bfEnd
			inF := c.Offset >= tensors[2].Offset && c.Offset < tensors[2].Offset+tensors[2].Length
			want := EncodingStored
			switch {
			case inBF:
				want = EncodingExpHuffmanV1
			case inF:
				want = wantF16
			}
			if c.Encoding != want {
				t.Errorf("%s: unit at %d is %s, want %s", name, c.Offset, c.Encoding, want)
			}
			if (inBF || inF) && want == EncodingExpHuffmanV1 && c.Length > ChunkBytes {
				t.Errorf("%s: encoded unit of %d bytes", name, c.Length)
			}
			if c.Length > modelfiles.ChunkSize {
				t.Errorf("%s: unit of %d bytes", name, c.Length)
			}
		}
		if got, err := unpackAll(t, packed, index, len(src), 4); err != nil || !bytes.Equal(got, src) {
			t.Fatalf("%s: unpack: %v", name, err)
		}
	}
	check("both dtypes decode faster than the disk", fast, EncodingExpHuffmanV1)
	check("F16 decodes slower than the disk", Rates{BF16: 15e9, F16: 4e9, Disk: 6.5e9}, EncodingStored)

	bf := weights(ChunkBytes, 23, bf16)
	for _, c := range []struct {
		name, dtype string
		rates       Rates
	}{
		{"quantised", "Q4_K", fast},
		{"F32", "F32", fast},
		{"BF16 slower than the disk", "BF16", Rates{BF16: 6e9, F16: 15e9, Disk: 7e9}},
		{"no rates", "BF16", Rates{}},
	} {
		index, packed := pack(t, bf, whole(bf, c.dtype), c.rates)
		if got := encodings(index); got[EncodingStored] != len(index.Chunks) {
			t.Errorf("%s: encodings %v, want all stored", c.name, got)
		}
		if !bytes.Equal(packed, bf) {
			t.Errorf("%s: stored pack differs from the object", c.name)
		}
	}
	// Uniform random bits save nothing and stay stored inside a BF16 tensor.
	noise := randomBytes(ChunkBytes, 22)
	if index, _ := pack(t, noise, whole(noise, "BF16"), fast); encodings(index)[EncodingStored] != 1 {
		t.Errorf("random BF16 bits: %v, want stored", encodings(index))
	}
	if _, err := Pack(io.Discard, make([]byte, 8), []Tensor{{Offset: 0, Length: 7, DType: "F16"}}, fast); !errors.Is(err, ErrDType) {
		t.Errorf("odd F16 tensor: %v, want ErrDType", err)
	}
	ts := func(o, l int64) Tensor { return Tensor{Offset: o, Length: l, DType: "BF16"} }
	for _, bad := range [][]Tensor{{ts(4, 4), ts(2, 2)}, {ts(0, 10)}, {ts(-2, 2)}} {
		if _, err := Pack(io.Discard, make([]byte, 8), bad, fast); err == nil {
			t.Errorf("tensors %v accepted", bad)
		}
	}
}

// INF-MP1: the index is recorded with these field names and words.
func TestIndexJSON(t *testing.T) {
	// 20,000 values: a unit this small saves 3 percent only when its 1,152
	// bytes of tables are small beside it (1,000 values stay stored).
	src := weights(20000, 24, bf16)
	index, _ := pack(t, src, whole(src, "BF16"), fast)
	b, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(index.Chunks[0].Packed)
	want := `{"digest":"sha256:` + spanSums(src)[0] + `","length":40000,"chunks":[{"offset":0,"length":40000,"packed_offset":0,"packed":` + string(p) +
		`,"encoding":"exp-huffman-v1"}],"spans":["` + spanSums(src)[0] + `"]}`
	if string(b) != want {
		t.Fatalf("index %s, want %s", b, want)
	}
}

// INF-MP4: bytes changed on disk are refused by the span digest, in an
// encoded unit's raw plane and in a stored unit alike, and in either span.
func TestCorruptUnitRefusedBySpanDigest(t *testing.T) {
	src, tensors := object()
	index, packed := pack(t, src, tensors, fast)
	var flips []int64
	for _, c := range index.Chunks {
		if c.Encoding == EncodingExpHuffmanV1 && len(flips) == 0 {
			flips = append(flips, c.PackedOffset+c.Packed-1) // last byte of the raw low plane
		}
	}
	last := index.Chunks[len(index.Chunks)-1]
	flips = append(flips, index.Chunks[0].PackedOffset+10, last.PackedOffset+last.Packed-1)
	for _, at := range flips {
		bad := bytes.Clone(packed)
		bad[at] ^= 0x10
		if _, err := unpackAll(t, bad, index, len(src), 3); !errors.Is(err, modelfiles.ErrDigest) {
			t.Errorf("byte %d flipped: %v, want modelfiles.ErrDigest", at, err)
		}
	}
	wrong := index
	wrong.Spans = []string{index.Spans[1], index.Spans[0]}
	if _, err := unpackAll(t, packed, wrong, len(src), 3); !errors.Is(err, modelfiles.ErrDigest) {
		t.Errorf("swapped span digests: %v, want modelfiles.ErrDigest", err)
	}
}

// INF-MP4: a pack cut short, anywhere, is refused.
func TestTruncatedPackRefused(t *testing.T) {
	src := weights(3*ChunkBytes/2, 27, bf16)
	index, packed := pack(t, src, whole(src, "BF16"), fast)
	for _, cut := range []int64{0, index.Chunks[1].PackedOffset + 3, int64(len(packed)) - 1} {
		if _, err := unpackAll(t, packed[:cut], index, len(src), 3); !errors.Is(err, ErrCorrupt) {
			t.Errorf("pack cut to %d of %d bytes: %v, want ErrCorrupt", cut, len(packed), err)
		}
	}
}

// INF-MP1: an index that does not tile the object, names an unknown
// encoding, or carries malformed or missing span digests is refused before
// anything is read.
func TestUnpackRefusesBadIndex(t *testing.T) {
	src := weights(ChunkBytes, 28, bf16)
	index, packed := pack(t, src, whole(src, "BF16"), fast)
	edits := map[string]func(x *Index){
		"gap":               func(x *Index) { x.Chunks[1].Offset++ },
		"short":             func(x *Index) { x.Chunks[1].Length-- },
		"unknown encoding":  func(x *Index) { x.Chunks[0].Encoding = "zstd" },
		"stored, two sizes": func(x *Index) { x.Chunks[0].Encoding = EncodingStored },
		"packed size huge":  func(x *Index) { x.Chunks[0].Packed = 1 << 40 },
		"upper-case hex":    func(x *Index) { x.Spans[0] = string(bytes.ToUpper([]byte(x.Spans[0]))) },
		"prefixed digest":   func(x *Index) { x.Spans[0] = "sha256:" + x.Spans[0][7:] },
		"missing span":      func(x *Index) { x.Spans = nil },
		"length":            func(x *Index) { x.Length += 2 },
	}
	for name, edit := range edits {
		bad := index
		bad.Chunks = slices.Clone(index.Chunks)
		bad.Spans = slices.Clone(index.Spans)
		edit(&bad)
		if _, err := unpackAll(t, packed, bad, len(src), 2); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v, want ErrCorrupt", name, err)
		}
	}
}

// INF-MP4: a cancelled context stops the decode with its error.
func TestUnpackCancelled(t *testing.T) {
	src := weights(ChunkBytes, 29, bf16)
	index, packed := pack(t, src, whole(src, "BF16"), fast)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Unpack(ctx, make([]byte, len(src)), bytes.NewReader(packed), index, index.Spans, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled unpack: %v", err)
	}
}

// INF-MP1, INF-MP4: the proof is the caller's record of the object. A pack
// rewritten together with its index's spans decodes to the rewritten bytes
// and passes against its own spans, and is refused against the spans OA
// recorded when it packed the object.
func TestUnpackTrustsOnlyTheRecordedSpans(t *testing.T) {
	src, tensors := object()
	recorded, packed := pack(t, src, tensors, fast)
	forged := bytes.Clone(src)
	forged[5] ^= 0xff // inside the stored header
	forgedIndex, forgedPack := pack(t, forged, tensors, fast)
	if slices.Equal(forgedIndex.Spans, recorded.Spans) {
		t.Fatal("the forged object has the recorded spans")
	}
	dst := make([]byte, len(src))
	if err := Unpack(context.Background(), dst, bytes.NewReader(forgedPack), forgedIndex, forgedIndex.Spans, 4); err != nil {
		t.Fatalf("forged pack against its own spans: %v", err)
	}
	err := Unpack(context.Background(), dst, bytes.NewReader(forgedPack), forgedIndex, recorded.Spans, 4)
	if !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("forged pack against the recorded spans: %v, want modelfiles.ErrDigest", err)
	}
	if err := Unpack(context.Background(), dst, bytes.NewReader(packed), recorded, nil, 4); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("no spans: %v, want ErrCorrupt", err)
	}
}

// INF-MP2: Pack decodes every encoded unit before writing it; an encoder
// whose output decodes to other bytes, or does not decode, refuses the pack.
func TestPackRefusesBrokenEncoder(t *testing.T) {
	src := weights(3*ChunkBytes/2, 30, bf16)
	defer func(e func([]byte, []byte, string) ([]byte, error)) { encodeUnit = e }(encodeUnit)
	for name, broken := range map[string]func([]byte) []byte{
		"flipped raw byte": func(p []byte) []byte { p[len(p)-1] ^= 1; return p },
		"cut short":        func(p []byte) []byte { return p[:len(p)-1] },
		"wrong count":      func(p []byte) []byte { p[4]++; return p },
	} {
		encodeUnit = func(dst, b []byte, dtype string) ([]byte, error) {
			p, err := EncodeChunk(dst, b, dtype)
			return broken(p), err
		}
		var w bytes.Buffer
		if _, err := Pack(&w, src, whole(src, "BF16"), fast); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Pack = %v, want ErrCorrupt", name, err)
		}
	}
}
