package modelpack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

type stTensor struct {
	name, dtype string
	data        []byte
}

// safetensorsFile builds a safetensors file: an 8-byte header length, the
// JSON header with __metadata__, then the tensors' bytes in order.
func safetensorsFile(tensors ...stTensor) []byte {
	header := map[string]any{"__metadata__": map[string]string{"format": "pt"}}
	var data []byte
	for _, t := range tensors {
		header[t.name] = map[string]any{"dtype": t.dtype, "shape": []int{len(t.data)}, "data_offsets": []int{len(data), len(data) + len(t.data)}}
		data = append(data, t.data...)
	}
	h, _ := json.Marshal(header)
	for len(h)%8 != 0 {
		h = append(h, ' ')
	}
	out := binary.LittleEndian.AppendUint64(nil, uint64(len(h)))
	return slices.Concat(out, h, data)
}

type ggufTensor struct {
	name string
	typ  uint32
	dims []uint64
	data []byte
}

// ggufFile builds a GGUF v3 file with a few key-values, including a string
// array, and the tensors' data aligned to 32 bytes.
func ggufFile(tensors ...ggufTensor) []byte {
	str := func(b []byte, s string) []byte {
		return append(binary.LittleEndian.AppendUint64(b, uint64(len(s))), s...)
	}
	b := []byte("GGUF")
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(tensors)))
	b = binary.LittleEndian.AppendUint64(b, 3)
	b = str(b, "general.architecture")
	b = binary.LittleEndian.AppendUint32(b, 8)
	b = str(b, "llama")
	b = str(b, "tokenizer.ggml.tokens")
	b = binary.LittleEndian.AppendUint32(b, 9)
	b = binary.LittleEndian.AppendUint32(b, 8)
	b = binary.LittleEndian.AppendUint64(b, 3)
	for _, s := range []string{"<s>", "a", "b"} {
		b = str(b, s)
	}
	b = str(b, "general.alignment")
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = binary.LittleEndian.AppendUint32(b, 32)
	var offsets []uint64
	var data []byte
	for _, t := range tensors {
		for len(data)%32 != 0 {
			data = append(data, 0)
		}
		offsets = append(offsets, uint64(len(data)))
		data = append(data, t.data...)
	}
	for i, t := range tensors {
		b = str(b, t.name)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(t.dims)))
		for _, d := range t.dims {
			b = binary.LittleEndian.AppendUint64(b, d)
		}
		b = binary.LittleEndian.AppendUint32(b, t.typ)
		b = binary.LittleEndian.AppendUint64(b, offsets[i])
	}
	for len(b)%32 != 0 {
		b = append(b, 0)
	}
	return append(b, data...)
}

// INF-MP9: tensor tables are read from safetensors and GGUF headers.
func TestReadTensors(t *testing.T) {
	bf := weights(1000, 90, bf16)
	st := safetensorsFile(stTensor{"a.weight", "BF16", bf}, stTensor{"b.bias", "F32", make([]byte, 40)}, stTensor{"c", "I64", make([]byte, 16)})
	format, got, err := ReadTensors(bytes.NewReader(st), int64(len(st)))
	if err != nil || format != "safetensors" || len(got) != 3 {
		t.Fatalf("safetensors: %s %v %v", format, got, err)
	}
	if got[0].DType != "BF16" || !bytes.Equal(st[got[0].Offset:got[0].Offset+got[0].Length], bf) {
		t.Fatalf("safetensors BF16 tensor at %+v", got[0])
	}
	f := weights(64, 91, f16)
	g := ggufFile(ggufTensor{"tok", 12, []uint64{256}, make([]byte, 144)}, ggufTensor{"norm", 1, []uint64{64}, f})
	format, got, err = ReadTensors(bytes.NewReader(g), int64(len(g)))
	if err != nil || format != "gguf" || len(got) != 1 || got[0].DType != "F16" {
		t.Fatalf("gguf: %s %v %v", format, got, err)
	}
	if !bytes.Equal(g[got[0].Offset:got[0].Offset+got[0].Length], f) {
		t.Fatal("gguf F16 tensor bytes")
	}
	for name, bad := range map[string][]byte{
		"short":                   []byte("GGUF"),
		"no JSON":                 append(binary.LittleEndian.AppendUint64(nil, 4), "abcd"...),
		"header past the end":     binary.LittleEndian.AppendUint64(nil, 1<<20),
		"tensor past the end":     st[:len(st)-1],
		"gguf version":            append([]byte("GGUF"), 9, 0, 0, 0, 0, 0, 0, 0),
		"gguf truncated":          g[:60],
		"pickle":                  {0x80, 0x02, 'c', 'b', 'u', 'i', 'l', 't'},
		"overlapping safetensors": overlapping(),
	} {
		if _, _, err := ReadTensors(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, ErrNotWeightFile) {
			t.Errorf("%s: %v, want ErrNotWeightFile", name, err)
		}
	}
}

func overlapping() []byte {
	h := []byte(`{"a":{"dtype":"F16","shape":[4],"data_offsets":[0,8]},"b":{"dtype":"F16","shape":[4],"data_offsets":[4,12]}}`)
	return slices.Concat(binary.LittleEndian.AppendUint64(nil, uint64(len(h))), h, make([]byte, 12))
}

// INF-MP9: Restore writes the object in order and proves it as it goes;
// a pack whose bytes changed fails it.
func TestRestore(t *testing.T) {
	src, tensors := object()
	index, packed := pack(t, src, tensors, Rates{BF16: 7e9, BF16Fixed3: 20e9, F16: 15e9, Disk: 6.5e9})
	var out bytes.Buffer
	if err := Restore(context.Background(), &out, bytes.NewReader(packed), index, 4); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), src) {
		t.Fatal("restored bytes differ")
	}
	bad := bytes.Clone(packed)
	bad[len(bad)-1] ^= 1
	if err := Restore(context.Background(), &out, bytes.NewReader(bad), index, 4); !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("changed pack: %v, want modelfiles.ErrDigest", err)
	}
	other := index
	sum := sha256.Sum256([]byte("other"))
	other.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if err := Restore(context.Background(), &out, bytes.NewReader(packed), other, 4); !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("index naming another digest: %v, want modelfiles.ErrDigest", err)
	}
}

// INF-MP9: the measurement gives every rate, briefly.
func TestMeasureRates(t *testing.T) {
	file := filepath.Join(t.TempDir(), "disk.bin")
	if err := os.WriteFile(file, make([]byte, 32<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := MeasureRates(context.Background(), file, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{r.BF16, r.F16, r.BF16Fixed3, r.Disk} {
		if !(v > 0) || math.IsInf(v, 0) {
			t.Fatalf("rates %+v", r)
		}
	}
	if _, err := MeasureRates(context.Background(), "relative", time.Millisecond); err == nil {
		t.Fatal("a relative disk file was accepted")
	}
}

// INF-MP9: opening the file is outside the disk clock. Readers whose open
// takes longer than the whole slot, as a virus scan of a fresh file can,
// still read a block each and give a positive rate; a measurement that
// reads nothing is an error, never a rate of 0.
func TestDiskRateOutlastsASlowOpen(t *testing.T) {
	const size = 6 * measureDiskBlock
	data := bytes.NewReader(make([]byte, size))
	slot := 20 * time.Millisecond
	var opens atomic.Int32
	slow := func() (io.ReaderAt, func(), error) {
		opens.Add(1)
		time.Sleep(3 * slot)
		return data, func() {}, nil
	}
	t0 := time.Now()
	rate, blocks, err := readRate(context.Background(), size, measureDiskBlock, slot, slow)
	withOpens := float64(blocks*measureDiskBlock) / time.Since(t0).Seconds()
	if err != nil || !(rate > 0) || math.IsInf(rate, 0) || blocks < measureDiskReaders {
		t.Fatalf("after a slow open: rate %v, %d blocks, %v", rate, blocks, err)
	}
	if opens.Load() != measureDiskReaders {
		t.Fatalf("%d readers opened, want %d", opens.Load(), measureDiskReaders)
	}
	// The opens took 12 slots; a clock that held them would give a lower rate.
	if rate <= withOpens {
		t.Fatalf("rate %v is not above %v, the rate with the opens on the clock", rate, withOpens)
	}
	// A file of one block has one reader, which reads it. It reads the
	// block again while a coarse clock still shows no time passed.
	before := opens.Load()
	if rate, blocks, err := readRate(context.Background(), measureDiskBlock, measureDiskBlock, 0, slow); err != nil || blocks < 1 || !(rate > 0) {
		t.Fatalf("one block: rate %v, %d blocks, %v", rate, blocks, err)
	}
	if n := opens.Load() - before; n != 1 {
		t.Fatalf("one block: %d readers opened, want 1", n)
	}
	for _, c := range [][2]int64{{measureDiskBlock - 1, measureDiskBlock}, {100, 0}} {
		if _, _, err := readRate(context.Background(), c[0], c[1], slot, slow); !errors.Is(err, errNoRate) {
			t.Fatalf("%d bytes in blocks of %d: %v, want errNoRate", c[0], c[1], err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if rate, _, err := readRate(ctx, size, measureDiskBlock, slot, slow); err == nil || rate != 0 {
		t.Fatalf("a cancelled measurement gave rate %v, %v", rate, err)
	}
	failing := func() (io.ReaderAt, func(), error) { return failReader{}, func() {}, nil }
	if rate, _, err := readRate(context.Background(), size, measureDiskBlock, slot, failing); err == nil || rate != 0 {
		t.Fatalf("reads that fail gave rate %v, %v", rate, err)
	}
}

// stillClock replaces the measurement clock with one that stands still for
// its first stuck readings, as Windows' clock does between its 0.5 ms
// steps, and then steps 1 ms per reading.
func stillClock(t *testing.T, stuck int64) {
	t.Helper()
	base := time.Now()
	var readings atomic.Int64
	prev := now
	t.Cleanup(func() { now = prev })
	now = func() time.Time {
		k := readings.Add(1)
		if k <= stuck {
			return base
		}
		return base.Add(time.Duration(k-stuck) * time.Millisecond)
	}
}

// INF-MP9: a read or a decode that times as 0 is repeated until the
// stopwatch counts, at least minMeasure or the slot, whichever is shorter,
// and above 0; the disk is read again from its start. With the clock
// standing still for the first readings, both still give a positive,
// finite rate.
func TestMeasurementsOutlastAStillClock(t *testing.T) {
	stillClock(t, 40)
	small := bytes.NewReader(randomBytes(1000, 85))
	open := func() (io.ReaderAt, func(), error) { return small, func() {}, nil }
	rate, blocks, err := readRate(context.Background(), 1000, 1000, 50*time.Millisecond, open)
	if err != nil || !(rate > 0) || math.IsInf(rate, 0) || blocks < 2 {
		t.Fatalf("a read that times as 0: rate %v, %d blocks, %v", rate, blocks, err)
	}
	stillClock(t, 40)
	if rate, err := decodeRate(context.Background(), weights(8192, 86, bf16), "BF16", Rates{BF16Fixed3: 1e18, Disk: 1}, 50*time.Millisecond); err != nil || !(rate > 0) || math.IsInf(rate, 0) {
		t.Fatalf("a decode that times as 0: rate %v, %v", rate, err)
	}
	// A slot of 0 still needs a time above 0.
	stillClock(t, 40)
	if rate, _, err := readRate(context.Background(), 1000, 1000, 0, open); err != nil || !(rate > 0) || math.IsInf(rate, 0) {
		t.Fatalf("slot 0: rate %v, %v", rate, err)
	}
}

// INF-MP9: a file under one 16 MiB block, a LoRA adapter's size, measures
// the disk by reading it whole once; an empty file is refused.
func TestDiskRateOfASmallFile(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "adapter.safetensors")
	if err := os.WriteFile(small, randomBytes(1_200_000, 84), 0o600); err != nil {
		t.Fatal(err)
	}
	rate, err := diskRate(context.Background(), small, 10*time.Millisecond)
	if err != nil || !(rate > 0) || math.IsInf(rate, 0) {
		t.Fatalf("a 1.2 MB file: rate %v, %v", rate, err)
	}
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := diskRate(context.Background(), empty, 10*time.Millisecond); err == nil {
		t.Fatal("an empty file measured the disk")
	}
}

type failReader struct{}

func (failReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("read failed") }
