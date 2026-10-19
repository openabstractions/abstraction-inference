package modelpack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
)

// intDiffOf is int-diff-v1 of tuned against base (INF-MP12).
func intDiffOf(tuned, base []byte) []byte {
	out := make([]byte, len(tuned))
	intDiff(out, tuned, base)
	return out
}

// tunedFrom moves most of base's values by a few units in the last place,
// more where the value is small, as a fine-tune does.
func tunedFrom(base []byte, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 3))
	out := bytes.Clone(base)
	for i := 0; i < len(out); i += 2 {
		v := binary.LittleEndian.Uint16(out[i:])
		e := int(v >> 7 & 0xff)
		spread := max(1, 130-e)
		binary.LittleEndian.PutUint16(out[i:], v+uint16(r.IntN(spread)-spread/2))
	}
	return out
}

func ansRoundTrip(t *testing.T, src []byte, dtype string, base []byte) []byte {
	t.Helper()
	p, err := EncodeExpANS(nil, src, dtype, base)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(src))
	if err := DecodeExpANS(got, p, base); err != nil {
		t.Fatalf("%d values: %v", len(src)/2, err)
	}
	if !bytes.Equal(got, src) {
		t.Fatalf("%d values: the decode differs", len(src)/2)
	}
	return p
}

// INF-MP11: plain BF16 and F16 units decode to their bytes for every
// length, the four states' tail included, with the exponent plane coded
// and smaller than the exp-huffman-v1 unit of the same values.
func TestExpANSRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 7, 8, 9, 4096, 100_003, ChunkBytes / 2} {
		src := weights(n, uint64(n), bf16)
		for i := 0; i < len(src); i += 2 * 331 {
			binary.LittleEndian.PutUint16(src[i:], 0x7fc1)
		}
		p := ansRoundTrip(t, src, "BF16", nil)
		if n >= 4096 {
			eh, err := EncodeChunk(nil, src, "BF16")
			if err != nil {
				t.Fatal(err)
			}
			if p[2] != planeANS || len(p) >= len(eh) {
				t.Fatalf("%d values: exponent mode %d, %d bytes against exp-huffman-v1's %d", n, p[2], len(p), len(eh))
			}
		}
		ansRoundTrip(t, weights(n, uint64(n)+1, f16FromBF16), "F16", nil)
	}
	ansRoundTrip(t, values16(0x3f80, 0x3f80, 0x3f80, 0x3f80, 0x3f80), "BF16", nil) // constant planes
	ansRoundTrip(t, randomBytes(2*50_000, 5), "BF16", nil)                         // raw planes
}

// INF-MP11: a delta unit decodes to its transform's words given its base,
// codes its planes under the base's exponents where that is smaller, and is
// refused without its base or with another length of base.
func TestExpANSDelta(t *testing.T) {
	base := weights(ChunkBytes/2, 21, bf16)
	tuned := tunedFrom(base, 22)
	words := intDiffOf(tuned, base)
	p := ansRoundTrip(t, words, "BF16", base)
	if p[1] != layoutDelta || (p[2] != planeANSCtx && p[3] != planeANSCtx) {
		t.Fatalf("layout %d, modes %d and %d: no plane took the base's contexts", p[1], p[2], p[3])
	}
	alone, err := EncodeExpANS(nil, words, "BF16", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) >= len(alone) {
		t.Fatalf("with contexts %d bytes, without %d", len(p), len(alone))
	}
	got := make([]byte, len(words))
	if err := DecodeExpANS(got, p, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("no base: %v", err)
	}
	if err := DecodeExpANS(got, p, base[:len(base)-2]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a short base: %v", err)
	}
	if _, err := EncodeExpANS(nil, words, "F16", base); !errors.Is(err, ErrDType) {
		t.Fatalf("an F16 delta: %v", err)
	}
	// One context holding one symbol takes the whole table (frequency 4096).
	same := bytes.Clone(base)
	ansRoundTrip(t, intDiffOf(same, base), "BF16", base)
}

// INF-MP11: a changed coded plane is refused: its table's sum or its
// stream's end state catches the change. (A raw plane, the low plane of
// plain weights, decodes changed bytes as exp-huffman-v1's does; the span
// hash refuses them.)
func TestExpANSRefusesChangedPayloads(t *testing.T) {
	src := weights(20_000, 31, bf16)
	p, err := EncodeExpANS(nil, src, "BF16", nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, len(src))
	for _, bad := range [][]byte{p[:len(p)-1], p[:ansHeaderLen], append(bytes.Clone(p), 0, 0)} {
		if err := DecodeExpANS(dst, bad, nil); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("a payload of %d bytes: %v", len(bad), err)
		}
	}
	if err := DecodeExpANS(dst[:len(dst)-2], p, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a short slot: %v", err)
	}
	r := rand.New(rand.NewPCG(32, 33))
	refused := 0
	hiLen := int(binary.LittleEndian.Uint32(p[8:]))
	if p[2] != planeANS {
		t.Fatalf("exponent plane mode %d", p[2])
	}
	for range 2000 {
		q := bytes.Clone(p)
		q[ansHeaderLen+r.IntN(hiLen)] ^= byte(1 << r.IntN(8))
		err := DecodeExpANS(dst, q, nil)
		if err != nil && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("refusal %v is not ErrCorrupt", err)
		}
		if err != nil {
			refused++
		}
	}
	if refused < 1990 {
		t.Fatalf("only %d of 2000 changed exponent planes refused", refused)
	}
}

// INF-MP1, INF-MP11: a pack whose index names exp-ans-v1 units decodes
// through Unpack and proves its spans; an ans unit stored in more than its
// length plus its header is refused before any read.
func TestUnpackReadsExpANS(t *testing.T) {
	src := weights(3*ChunkBytes/2, 61, bf16)
	var packed []byte
	var units []ChunkEntry
	for off := 0; off < len(src); off += ChunkBytes {
		u := src[off:min(off+ChunkBytes, len(src))]
		p := mustANS(u, "BF16", nil)
		units = append(units, ChunkEntry{Offset: int64(off), Length: int64(len(u)), PackedOffset: int64(len(packed)), Packed: int64(len(p)), Encoding: EncodingExpANSV1})
		packed = append(packed, p...)
	}
	index := Index{Length: int64(len(src)), Chunks: units, Spans: spanDigests(src)}
	got, err := unpackAll(t, packed, index, len(src), 2)
	if err != nil || !bytes.Equal(got, src) {
		t.Fatalf("Unpack of exp-ans-v1 units: %v", err)
	}
	index.Chunks[0].Packed = ansHeaderLen + index.Chunks[0].Length + 1
	if _, err := checkIndex(index); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("an oversize ans unit: %v", err)
	}
}

// lowStatePayload is the review's payload: a one-value BF16 unit whose
// exponent plane holds a one-symbol table and a stream that starts state 0
// at 1, below 2^16, then reads one word 0; it reaches 2^16 at the end.
func lowStatePayload() []byte {
	var t ansTable
	t.freq[0] = ansScale
	plane := appendTable(nil, &t)
	plane = binary.LittleEndian.AppendUint32(plane, 1)
	for range ansStates - 1 {
		plane = binary.LittleEndian.AppendUint32(plane, ansL)
	}
	plane = binary.LittleEndian.AppendUint16(plane, 0)
	p := []byte{ansVersion, dtypeBF16, planeANS, planeRaw}
	p = binary.LittleEndian.AppendUint32(p, 1)
	p = binary.LittleEndian.AppendUint32(p, uint32(len(plane)))
	p = binary.LittleEndian.AppendUint32(p, 1)
	return append(append(p, plane...), 0x3f)
}

// INF-MP11: a stream that starts a state below 2^16 was written by no
// encoder and is refused, though it would end at 2^16 with no word left.
func TestANSRefusesALowStartingState(t *testing.T) {
	if err := DecodeExpANS(make([]byte, 2), lowStatePayload(), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("a starting state of 1: %v", err)
	}
}

// INF-MP11: tables whose frequencies do not sum to the scale, pass 256
// symbols, or end early are refused.
func TestANSTables(t *testing.T) {
	var t0 ansTable
	hist := [256]uint64{3: 10, 200: 30}
	if !normalize(&hist, &t0) || t0.freq[3]+t0.freq[200] != ansScale {
		t.Fatalf("normalised %d and %d", t0.freq[3], t0.freq[200])
	}
	good := appendTable(nil, &t0)
	if s, n, err := readTable(good); err != nil || s == nil || n != len(good) {
		t.Fatalf("a table: %v", err)
	}
	for name, b := range map[string][]byte{
		"empty":         nil,
		"run past 256":  binary.AppendUvarint(nil, 257),
		"frequency max": binary.AppendUvarint(binary.AppendUvarint(nil, 0), ansScale),
		"sum short":     binary.AppendUvarint(binary.AppendUvarint(binary.AppendUvarint(nil, 0), 99), 255),
		"cut":           good[:len(good)-1],
	} {
		if _, _, err := readTable(b); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if s, _, err := readTable(appendTable(nil, nil)); err != nil || s != nil {
		t.Fatalf("an absent table: %v", err)
	}
}

// FuzzDecodeExpANS feeds the decoder arbitrary payloads, hostile tables,
// states and context headers included, with and without a base. It must
// refuse with ErrCorrupt or decode without panicking or writing outside
// its slot.
func FuzzDecodeExpANS(f *testing.F) {
	base := weights(300, 41, bf16)
	for _, seed := range [][]byte{
		mustANS(weights(300, 42, bf16), "BF16", nil),
		mustANS(weights(5, 43, bf16), "BF16", nil),
		mustANS(intDiffOf(tunedFrom(base, 44), base), "BF16", base),
		mustANS(values16(1, 1, 1), "BF16", nil),
		mustANS(randomBytes(64, 45), "BF16", nil),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, p []byte) {
		n := 300
		if len(p) >= 8 {
			if c := int(binary.LittleEndian.Uint32(p[4:])); c > 0 && c <= 1<<16 {
				n = c
			}
		}
		b := weights(n, 46, bf16)
		for _, withBase := range []bool{false, true} {
			buf := make([]byte, 2*n+64)
			dst := buf[32 : 32+2*n : 32+2*n]
			var err error
			if withBase {
				err = DecodeExpANS(dst, p, b)
			} else {
				err = DecodeExpANS(dst, p, nil)
			}
			for i, x := range append(buf[:32:32], buf[32+2*n:]...) {
				if x != 0 {
					t.Fatalf("guard byte %d written", i)
				}
			}
			if err != nil && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("refusal %v is not ErrCorrupt", err)
			}
		}
	})
}

func benchmarkANS(b *testing.B, words, base []byte) {
	p := mustANS(words, "BF16", base)
	dst := make([]byte, len(words))
	b.SetBytes(int64(len(words)))
	b.ResetTimer()
	for range b.N {
		if err := DecodeExpANS(dst, p, base); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeExpANSPlain(b *testing.B) {
	benchmarkANS(b, weights(ChunkBytes/2, 51, bf16), nil)
}

func BenchmarkDecodeExpANSDelta(b *testing.B) {
	base := weights(ChunkBytes/2, 52, bf16)
	benchmarkANS(b, intDiffOf(tunedFrom(base, 53), base), base)
}

func BenchmarkDecodeExpHuffman(b *testing.B) {
	src := weights(ChunkBytes/2, 51, bf16)
	p, _ := EncodeChunk(nil, src, "BF16")
	dst := make([]byte, len(src))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		if err := DecodeChunk(dst, p); err != nil {
			b.Fatal(err)
		}
	}
}

func mustANS(src []byte, dtype string, base []byte) []byte {
	p, err := EncodeExpANS(nil, src, dtype, base)
	if err != nil {
		panic(err)
	}
	return p
}
