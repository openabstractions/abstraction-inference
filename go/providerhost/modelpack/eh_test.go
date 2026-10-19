package modelpack

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
)

type encoded struct {
	payload        []byte
	hiMode, loMode byte
}

func encode(t *testing.T, src []byte, dtype string) encoded {
	t.Helper()
	p, err := EncodeChunk(nil, src, dtype)
	if err != nil {
		t.Fatalf("EncodeChunk: %v", err)
	}
	return encoded{p, p[2], p[3]}
}

func roundTrip(t *testing.T, src []byte, dtype string) encoded {
	t.Helper()
	e := encode(t, src, dtype)
	dst := make([]byte, len(src))
	for i := range dst {
		dst[i] = 0xa5
	}
	if err := DecodeChunk(dst, e.payload); err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if !bytes.Equal(dst, src) {
		i := 0
		for dst[i] == src[i] {
			i++
		}
		t.Fatalf("decoded bytes differ from byte %d of %d (value %d)", i, len(src), i/2)
	}
	return e
}

// INF-MP3: every 16-bit pattern a tensor may hold returns bit for bit,
// whichever plane modes the encoder chose, at every tail length.
func TestRoundTripBitExact(t *testing.T) {
	special := []uint16{
		0x0000, 0x8000, // +0, -0
		0x7f80, 0xff80, // BF16 +Inf, -Inf
		0x7fc0, 0xffc1, 0x7f81, // BF16 quiet and signalling NaNs with payloads
		0x0001, 0x8001, 0x007f, // BF16 subnormals
		0x7c00, 0xfc00, 0x7e00, 0x7c01, // F16 Inf and NaNs
		0x0001, 0x03ff, 0x83ff, // F16 subnormals
		0x7f7f, 0xff7f, 0x7bff, // largest finite
	}
	mixed := weights(ChunkBytes/2, 1, bf16)
	for i, v := range special {
		binary.LittleEndian.PutUint16(mixed[2*(i*997):], v)
	}
	sameExponent := make([]byte, 2*5000)
	for i := range 5000 {
		// exponent 120, every mantissa and both signs
		binary.LittleEndian.PutUint16(sameExponent[2*i:], uint16(i%2)<<15|120<<7|uint16(i%128))
	}
	cases := []struct {
		name, dtype    string
		src            []byte
		hiMode, loMode byte
	}{
		{"BF16 weights, full chunk", "BF16", weights(ChunkBytes/2, 2, bf16), planeHuffman, planeRaw},
		{"BF16 weights with NaN, Inf and subnormals", "BF16", mixed, planeHuffman, planeRaw},
		{"F16 weights", "F16", weights(ChunkBytes/2, 3, f16), planeHuffman, planeRaw},
		{"F16 converted from BF16", "F16", weights(ChunkBytes/2, 4, f16FromBF16), planeHuffman, planeHuffman},
		{"one exponent in the whole chunk", "BF16", sameExponent, planeConst, planeRaw},
		{"one value repeated", "BF16", values16(slices.Repeat([]uint16{0x3c00}, 3001)...), planeConst, planeConst},
		{"special values only", "BF16", values16(special...), 255, 255},
		{"uniform random bits", "BF16", randomBytes(2*70001, 5), planeRaw, planeRaw},
		{"largest chunk", "BF16", weights(maxChunkValues, 6, bf16), planeHuffman, planeRaw},
		{"the full-unit fuzz seed", "BF16", fullUnit(), planeHuffman, planeHuffman},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := roundTrip(t, c.src, c.dtype)
			if c.hiMode != 255 && (e.hiMode != c.hiMode || e.loMode != c.loMode) {
				t.Fatalf("plane modes %d/%d, want %d/%d", e.hiMode, e.loMode, c.hiMode, c.loMode)
			}
		})
	}
	// Every tail length around the substream and quad boundaries, through
	// both decoding paths.
	for _, n := range []int{1, 2, 3, 4, 5, 7, 255, 256, 1023, 1024, 1025, 1027, 2048 + 3, 8*256*4 + 1, 8*256*4 - 5} {
		roundTrip(t, weights(n, uint64(n), bf16), "BF16")
		roundTrip(t, weights(n, uint64(n), f16FromBF16), "F16")
	}
}

// INF-MP2: the encoder takes 16-bit floats only.
func TestEncoderRefusesOtherInput(t *testing.T) {
	for _, c := range []struct {
		dtype string
		src   []byte
	}{
		{"F32", make([]byte, 64)},
		{"Q4_K", make([]byte, 64)},
		{"bf16", make([]byte, 64)},
		{"BF16", make([]byte, 63)},
		{"BF16", nil},
		{"F16", make([]byte, 2*maxChunkValues+2)},
	} {
		if _, err := EncodeChunk(nil, c.src, c.dtype); !errors.Is(err, ErrDType) {
			t.Errorf("EncodeChunk(%q, %d bytes) = %v, want ErrDType", c.dtype, len(c.src), err)
		}
	}
}

// INF-MP4: a payload cut anywhere, or with a byte added, is refused.
func TestDecodeRefusesTruncatedOrLong(t *testing.T) {
	for _, src := range [][]byte{weights(9000, 8, bf16), weights(9000, 9, f16FromBF16)} {
		e := encode(t, src, "BF16")
		dst := make([]byte, len(src))
		for cut := 0; cut < len(e.payload); cut += 1 + cut/50 {
			if err := DecodeChunk(dst, e.payload[:cut]); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("payload cut to %d of %d bytes: %v, want ErrCorrupt", cut, len(e.payload), err)
			}
		}
		if err := DecodeChunk(dst, append(bytes.Clone(e.payload), 0)); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("payload with a trailing byte: %v, want ErrCorrupt", err)
		}
		if err := DecodeChunk(dst[:len(dst)-2], e.payload); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("slot one value short: %v, want ErrCorrupt", err)
		}
	}
}

// INF-MP3: the substream offsets let decoders start mid-chunk; decoders
// over disjoint ranges, run side by side, give the bytes of one decoder.
func TestDecodeFromMidChunk(t *testing.T) {
	for _, src := range [][]byte{
		weights(ChunkBytes/2-3, 10, bf16),
		weights(ChunkBytes/2-1, 11, f16FromBF16),
	} {
		e := encode(t, src, "BF16")
		var c chunk
		if err := c.parse(e.payload); err != nil {
			t.Fatal(err)
		}
		dst := make([]byte, len(src))
		cuts := []int{0, 3, 37, 64, 200, 255, 256}
		var wg sync.WaitGroup
		errs := make([]error, len(cuts)-1)
		for i := range errs {
			wg.Go(func() { errs[i] = c.decode(dst, cuts[i], cuts[i+1]) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dst, src) {
			t.Fatal("decoders started mid-chunk give other bytes")
		}
	}
}

// INF-MP3: a substream whose bits run past its recorded end is refused.
func TestDecodeRefusesSubstreamOverrun(t *testing.T) {
	src := weights(20000, 12, bf16)
	e := encode(t, src, "BF16")
	p := bytes.Clone(e.payload)
	// Move the end of substream 10 one byte earlier and substream 11's start
	// with it: substream 10 now ends before its bits do.
	at := headerLen + lensLen + 4*10
	binary.LittleEndian.PutUint32(p[at:], binary.LittleEndian.Uint32(p[at:])-1)
	if err := DecodeChunk(make([]byte, len(src)), p); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("shifted substream end: %v, want ErrCorrupt", err)
	}
	// An incomplete code: one length lengthened.
	p = bytes.Clone(e.payload)
	for i := headerLen; i < headerLen+lensLen; i++ {
		if p[i]&15 != 0 && p[i]&15 < maxCodeLen {
			p[i]++
			break
		}
	}
	if err := DecodeChunk(make([]byte, len(src)), p); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("incomplete code: %v, want ErrCorrupt", err)
	}
}

// BenchmarkDecodeChunk decodes one 512 KiB chunk on one goroutine.
func BenchmarkDecodeChunk(b *testing.B) {
	for _, c := range []struct {
		name    string
		convert func(float32) uint16
		dtype   string
	}{
		{"BF16", bf16, "BF16"},
		{"F16", f16, "F16"},
		{"F16FromBF16", f16FromBF16, "F16"},
	} {
		b.Run(c.name, func(b *testing.B) {
			src := weights(ChunkBytes/2, 1, c.convert)
			p, err := EncodeChunk(nil, src, c.dtype)
			if err != nil {
				b.Fatal(err)
			}
			dst := make([]byte, len(src))
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				if err := DecodeChunk(dst, p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A length-limited code still fits 12 bits when counts are very skewed.
func TestCodeLengthsLimited(t *testing.T) {
	var freq [256]uint64
	f := uint64(1)
	for i := range 40 {
		freq[i] = f
		f = f*2 + 1
	}
	var lens [256]uint8
	codeLengths(&freq, &lens)
	var tab decodeTable
	if !buildTable(&lens, &tab) {
		t.Fatalf("lengths %v are not a complete code of at most 12 bits", lens[:40])
	}
}
