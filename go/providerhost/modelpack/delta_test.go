package modelpack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// deltaObject is a model of a header, a BF16 tensor "w" of 2.5 MiB, an F32
// tensor and a BF16 tensor "v"; seed varies the BF16 values.
func deltaObject(header []byte, w, v []byte) ([]byte, []Tensor) {
	f32 := randomBytes(4096, 70)
	obj := slices.Concat(header, w, f32, v)
	at := int64(len(header))
	return obj, []Tensor{
		{Name: "w", DType: "BF16", Offset: at, Length: int64(len(w))},
		{Name: "x", DType: "F32", Offset: at + int64(len(w)), Length: int64(len(f32))},
		{Name: "v", DType: "BF16", Offset: at + int64(len(w)) + int64(len(f32)), Length: int64(len(v))},
	}
}

// deltaPair is a base and a fine-tune of it: the fine-tune moves "w" and
// "v" a little and has a longer header, so its tensors lie at other offsets
// than the base's.
func deltaPair() (base []byte, baseTensors []Tensor, tuned []byte, tunedTensors []Tensor) {
	w := weights(5*ChunkBytes/4, 71, bf16)
	v := weights(4000, 72, bf16)
	base, baseTensors = deltaObject(randomBytes(900, 73), w, v)
	tuned, tunedTensors = deltaObject(randomBytes(1300, 74), tunedFrom(w, 75), tunedFrom(v, 76))
	return
}

func packDelta(t *testing.T, tuned []byte, tensors []Tensor, base []byte, baseTensors []Tensor) (Index, []byte) {
	t.Helper()
	var b bytes.Buffer
	index, err := PackDelta(&b, tuned, tensors, Rates{}, &DeltaBase{Base: ProveBase(base), Tensors: baseTensors})
	if err != nil {
		t.Fatal(err)
	}
	return index, b.Bytes()
}

// countingReader counts the reads that reach it.
type countingReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return c.r.ReadAt(p, off)
}

// INF-MP12: a fine-tune packed against its base holds delta units for the
// tensors the base holds by name, names the base, and unpacks and restores
// to its bytes against the base; with no base it is refused.
func TestPackDeltaRoundTrip(t *testing.T) {
	base, baseTensors, tuned, tensors := deltaPair()
	index, packed := packDelta(t, tuned, tensors, base, baseTensors)
	deltas := 0
	for _, c := range index.Chunks {
		if c.Transform == TransformIntDiffV1 {
			deltas++
		}
	}
	if deltas < 4 || index.Base == nil || index.Base.Digest != ProveBase(base).Digest() || index.Base.Length != int64(len(base)) {
		t.Fatalf("%d delta units, base %+v", deltas, index.Base)
	}
	var size int64
	for _, c := range index.Chunks {
		size += c.Packed
	}
	if size >= int64(len(tuned))*6/10 {
		t.Fatalf("packed %d of %d bytes", size, len(tuned))
	}
	dst := make([]byte, len(tuned))
	if err := UnpackWithBase(context.Background(), dst, bytes.NewReader(packed), index, index.Spans, ProveBase(base), 3); err != nil || !bytes.Equal(dst, tuned) {
		t.Fatalf("unpack against the base: %v", err)
	}
	var out bytes.Buffer
	if err := RestoreWithBase(context.Background(), &out, bytes.NewReader(packed), index, ProveBase(base), 2); err != nil || !bytes.Equal(out.Bytes(), tuned) {
		t.Fatalf("restore against the base: %v", err)
	}
	if err := Unpack(context.Background(), dst, bytes.NewReader(packed), index, index.Spans, 3); !errors.Is(err, ErrBase) {
		t.Fatalf("unpack with no base: %v", err)
	}
	if err := Restore(context.Background(), io.Discard, bytes.NewReader(packed), index, 2); !errors.Is(err, ErrBase) {
		t.Fatalf("restore with no base: %v", err)
	}
}

// INF-MP12: a base of another digest is refused before any unit is read.
func TestHostileBaseRefusedBeforeDecoding(t *testing.T) {
	base, baseTensors, tuned, tensors := deltaPair()
	index, packed := packDelta(t, tuned, tensors, base, baseTensors)
	other := bytes.Clone(base)
	other[len(other)/2] ^= 1
	src := &countingReader{r: bytes.NewReader(packed)}
	dst := make([]byte, len(tuned))
	err := UnpackWithBase(context.Background(), dst, src, index, index.Spans, ProveBase(other), 3)
	if !errors.Is(err, modelfiles.ErrDigest) || src.n.Load() != 0 {
		t.Fatalf("a base of another digest: %v after %d reads", err, src.n.Load())
	}
	err = RestoreWithBase(context.Background(), io.Discard, src, index, ProveBase(base[:len(base)-2]), 2)
	if !errors.Is(err, modelfiles.ErrDigest) || src.n.Load() != 0 {
		t.Fatalf("a base of another length: %v after %d reads", err, src.n.Load())
	}
}

// INF-MP12: a delta decoded against another base than the one it was made
// from, though that base matches the index's claim, fails the span proof.
func TestDeltaAgainstAnotherBaseFailsTheProof(t *testing.T) {
	base, baseTensors, tuned, tensors := deltaPair()
	index, packed := packDelta(t, tuned, tensors, base, baseTensors)
	other := bytes.Clone(base)
	for i := 1000; i < len(other); i += 2 * 4099 {
		other[i] ^= 0x01
	}
	index.Base = &BaseRef{Digest: ProveBase(other).Digest(), Length: int64(len(other))}
	dst := make([]byte, len(tuned))
	if err := UnpackWithBase(context.Background(), dst, bytes.NewReader(packed), index, index.Spans, ProveBase(other), 3); !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("unpack against another base: %v", err)
	}
	if err := RestoreWithBase(context.Background(), io.Discard, bytes.NewReader(packed), index, ProveBase(other), 2); !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("restore against another base: %v", err)
	}
}

// INF-MP12: a base that matches the index's claim but whose exponents
// differ from the base the delta was made from gives the coder other
// contexts; the decoder refuses before the proof, and its refusal names
// that cause beside damage.
func TestDeltaAgainstAnotherBaseNamesTheBase(t *testing.T) {
	base, baseTensors, tuned, tensors := deltaPair()
	index, packed := packDelta(t, tuned, tensors, base, baseTensors)
	other := bytes.Clone(base)
	for i := 1001; i < len(other); i += 2 * 97 {
		other[i] ^= 0x08 // the exponent's fourth bit, in each value's high byte
	}
	index.Base = &BaseRef{Digest: ProveBase(other).Digest(), Length: int64(len(other))}
	const hint = "or not the base this delta was made from"
	dst := make([]byte, len(tuned))
	err := UnpackWithBase(context.Background(), dst, bytes.NewReader(packed), index, index.Spans, ProveBase(other), 3)
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), hint) {
		t.Fatalf("unpack against a base of other exponents: %v", err)
	}
	err = RestoreWithBase(context.Background(), io.Discard, bytes.NewReader(packed), index, ProveBase(other), 2)
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), hint) {
		t.Fatalf("restore against a base of other exponents: %v", err)
	}
}

// INF-MP12: the index rules for delta units.
func TestDeltaIndexRules(t *testing.T) {
	base, baseTensors, tuned, tensors := deltaPair()
	good, _ := packDelta(t, tuned, tensors, base, baseTensors)
	first := slices.IndexFunc(good.Chunks, func(c ChunkEntry) bool { return c.Transform != "" })
	plain := slices.IndexFunc(good.Chunks, func(c ChunkEntry) bool { return c.Transform == "" })
	edit := func(f func(x *Index)) Index {
		x := good
		x.Chunks = slices.Clone(good.Chunks)
		b := *good.Base
		x.Base = &b
		f(&x)
		return x
	}
	for name, x := range map[string]Index{
		"no base":             edit(func(x *Index) { x.Base = nil }),
		"another transform":   edit(func(x *Index) { x.Chunks[first].Transform = "xor-v1" }),
		"another coder":       edit(func(x *Index) { x.Chunks[first].Encoding = EncodingExpHuffmanV1 }),
		"past the base":       edit(func(x *Index) { x.Chunks[first].BaseOffset = x.Base.Length }),
		"negative offset":     edit(func(x *Index) { x.Chunks[first].BaseOffset = -2 }),
		"offset, no delta":    edit(func(x *Index) { x.Chunks[plain].BaseOffset = 2 }),
		"bad base digest":     edit(func(x *Index) { x.Base.Digest = "sha256:00" }),
		"empty base":          edit(func(x *Index) { x.Base.Length = 0 }),
		"base, no delta unit": edit(func(x *Index) { x.Chunks = slices.Clone(good.Chunks); stripDeltas(x) }),
	} {
		if _, err := checkIndex(x); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := checkIndex(good); err != nil {
		t.Fatal(err)
	}
}

func stripDeltas(x *Index) {
	for i := range x.Chunks {
		if x.Chunks[i].Transform != "" {
			x.Chunks[i].Transform, x.Chunks[i].BaseOffset, x.Chunks[i].Encoding = "", 0, EncodingExpANSV1
		}
	}
}

// FuzzDeltaIndex feeds a delta index arbitrary JSON and unpacks what the
// index check accepts against a base of the length the index names, with
// arbitrary packed bytes. Nothing may panic; refusals are ErrCorrupt,
// ErrBase or modelfiles.ErrDigest.
func FuzzDeltaIndex(f *testing.F) {
	base := weights(20_000, 81, bf16)
	tuned := tunedFrom(base, 82)
	var b bytes.Buffer
	index, err := PackDelta(&b, tuned, []Tensor{{Name: "w", DType: "BF16", Offset: 0, Length: int64(len(tuned))}}, Rates{},
		&DeltaBase{Base: ProveBase(base), Tensors: []Tensor{{Name: "w", DType: "BF16", Offset: 0, Length: int64(len(base))}}})
	if err != nil {
		f.Fatal(err)
	}
	seed, _ := json.Marshal(index)
	f.Add(seed, b.Bytes())
	f.Add([]byte(`{"length":4,"chunks":[{"offset":0,"length":4,"packed":20,"encoding":"exp-ans-v1","transform":"int-diff-v1","base_offset":2}],"base":{"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","length":6}}`), make([]byte, 20))
	f.Fuzz(func(t *testing.T, raw, packed []byte) {
		var x Index
		if json.Unmarshal(raw, &x) != nil {
			return
		}
		if _, err := checkIndex(x); err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("index refusal %v is not ErrCorrupt", err)
			}
			return
		}
		if x.Length > 1<<20 || x.Base != nil && x.Base.Length > 1<<20 {
			return
		}
		var proven ProvenBase
		if x.Base != nil {
			bb := make([]byte, x.Base.Length)
			copy(bb, base)
			proven = ProveBase(bb)
			x.Base.Digest = proven.Digest()
		}
		dst := make([]byte, x.Length)
		err := UnpackWithBase(context.Background(), dst, bytes.NewReader(packed), x, x.Spans, proven, 2)
		if err != nil && !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrBase) && !errors.Is(err, modelfiles.ErrDigest) {
			t.Fatalf("refusal %v", err)
		}
	})
}
