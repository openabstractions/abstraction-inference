package modelpack

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestMeasure prints the codec's ratio and decode rates on this machine. It
// runs only with OA_MODELPACK_MEASURE=1, on the CPU, in memory:
//
//	OA_MODELPACK_MB           synthetic sample size in MiB (default 256)
//	OA_MODELPACK_GGUF         a GGUF file whose F16 and BF16 tensors, up to
//	                          512 MiB, are read (read only)
//	OA_MODELPACK_SAFETENSORS  a safetensors file read the same way
//
// The record is research/model-locators/GPU-AND-MATRIX-DECOMPRESSION.md §11.
func TestMeasure(t *testing.T) {
	if os.Getenv("OA_MODELPACK_MEASURE") != "1" {
		t.Skip("set OA_MODELPACK_MEASURE=1 to measure")
	}
	mb := 256
	if s := os.Getenv("OA_MODELPACK_MB"); s != "" {
		mb, _ = strconv.Atoi(s)
	}
	t.Logf("GOMAXPROCS %d, NumCPU %d, %s", runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.Version())
	n := mb << 19
	measure(t, "synthetic BF16, N(0, 0.02) with 1/64 from N(0, 0.3)", parallelWeights(n, bf16), "BF16")
	measure(t, "synthetic F16, same distribution (FP16-trained shape)", parallelWeights(n, f16), "F16")
	measure(t, "synthetic F16 rounded through BF16 (BF16-origin F16)", parallelWeights(n, f16FromBF16), "F16")
	if p := os.Getenv("OA_MODELPACK_GGUF"); p != "" {
		f16s, bf16s := readGGUF(t, p, 512<<20)
		measure(t, "real F16, "+p, f16s, "F16")
		if len(bf16s) > 0 {
			measure(t, "real BF16, "+p, bf16s, "BF16")
		}
		measure(t, "BF16 rounded from that real F16", toBF16(f16s), "BF16")
	}
	if p := os.Getenv("OA_MODELPACK_SAFETENSORS"); p != "" {
		f16s, bf16s := readSafetensors(t, p, 512<<20)
		if len(f16s) > 0 {
			measure(t, "real F16, "+p, f16s, "F16")
			measure(t, "BF16 rounded from that real F16", toBF16(f16s), "BF16")
		}
		if len(bf16s) > 0 {
			measure(t, "real BF16, "+p, bf16s, "BF16")
		}
	}
}

func parallelWeights(n int, convert func(float32) uint16) []byte {
	out := make([]byte, 2*n)
	parts := runtime.GOMAXPROCS(0)
	per := (n + parts - 1) / parts
	done := make(chan struct{})
	for p := range parts {
		go func() {
			lo, hi := min(p*per, n), min((p+1)*per, n)
			copy(out[2*lo:2*hi], weights(hi-lo, uint64(p)+100, convert))
			done <- struct{}{}
		}()
	}
	for range parts {
		<-done
	}
	return out
}

func toBF16(f16s []byte) []byte {
	out := make([]byte, len(f16s))
	for i := 0; i+1 < len(f16s); i += 2 {
		binary.LittleEndian.PutUint16(out[i:], bf16(f16ToF32(binary.LittleEndian.Uint16(f16s[i:]))))
	}
	return out
}

func measure(t *testing.T, name string, src []byte, dtype string) {
	if len(src) == 0 {
		return
	}
	t0 := time.Now()
	var b bytes.Buffer
	b.Grow(len(src))
	index, err := Pack(&b, src, whole(src, dtype), fast)
	if err != nil {
		t.Fatal(err)
	}
	encode := time.Since(t0)
	packed := b.Bytes()
	modes := map[string]int{}
	for _, c := range index.Chunks {
		if c.Encoding == EncodingExpHuffmanV1 {
			p := packed[c.PackedOffset:]
			modes[fmt.Sprintf("high %s, low %s", modeName(p[2]), modeName(p[3]))]++
		}
	}
	enc, _ := json.Marshal(encodings(index))
	t.Logf("%s: %.1f MiB, kept %.4f, chunks %s, plane modes %v, encode %.2f GB/s (all threads)",
		name, float64(len(src))/(1<<20), float64(len(packed))/float64(len(src)), enc, modes, gbps(len(src), encode))

	dst := make([]byte, len(src))
	for i := 0; i < len(dst); i += 4096 {
		dst[i] = 1 // touch every page before timing
	}
	mem := func(c *ChunkEntry, _ []byte) ([]byte, error) {
		return packed[c.PackedOffset : c.PackedOffset+c.Packed], nil
	}
	line := ""
	for _, w := range []int{1, 8, 16, 32} {
		var rates [2]float64
		for k, verify := range []bool{false, true} {
			runs := 7
			if w == 1 {
				runs = 3
			}
			best := time.Duration(math.MaxInt64)
			for range runs {
				t0 := time.Now()
				if err := unpack(context.Background(), dst, index, index.Spans, w, verify, func() (readFunc, func(), error) { return mem, func() {}, nil }, nil, nil, nil); err != nil {
					t.Fatal(err)
				}
				best = min(best, time.Since(t0))
			}
			rates[k] = gbps(len(src), best)
		}
		line += fmt.Sprintf(" | %d: %.2f (%.2f with span SHA-256)", w, rates[0], rates[1])
	}
	if !bytes.Equal(dst, src) {
		t.Fatalf("%s: decoded bytes differ", name)
	}
	t.Logf("%s: decode GB/s by goroutines%s", name, line)
}

func modeName(m byte) string {
	return [...]string{"raw", "coded", "constant"}[m]
}

func gbps(n int, d time.Duration) float64 { return float64(n) / 1e9 / d.Seconds() }

// readGGUF returns up to limit bytes of the file's F16 tensors and of its
// BF16 tensors, each concatenated in file order (GGUF v2 and v3).
func readGGUF(t *testing.T, path string, limit int) (f16s, bf16s []byte) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var pos int64
	read := func(v any) {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			t.Fatalf("%s: header: %v", path, err)
		}
		pos += int64(binary.Size(v))
	}
	str := func() string {
		var n uint64
		read(&n)
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatal(err)
		}
		pos += int64(n)
		return string(b)
	}
	var magic [4]byte
	var version uint32
	var tensors, kvs uint64
	read(&magic)
	read(&version)
	read(&tensors)
	read(&kvs)
	if string(magic[:]) != "GGUF" {
		t.Fatalf("%s is not GGUF", path)
	}
	size := map[uint32]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}
	align := uint64(32)
	var value func(typ uint32) uint64
	value = func(typ uint32) uint64 {
		switch typ {
		case 8:
			str()
		case 9:
			var et uint32
			var n uint64
			read(&et)
			read(&n)
			for range n {
				value(et)
			}
		default:
			b := make([]byte, size[typ])
			if _, err := io.ReadFull(r, b); err != nil {
				t.Fatal(err)
			}
			pos += int64(len(b))
			if len(b) == 4 {
				return uint64(binary.LittleEndian.Uint32(b))
			}
		}
		return 0
	}
	for range kvs {
		key := str()
		var typ uint32
		read(&typ)
		if v := value(typ); key == "general.alignment" {
			align = v
		}
	}
	type info struct {
		typ          uint32
		offset, size uint64
	}
	var infos []info
	for range tensors {
		str()
		var dims uint32
		read(&dims)
		count := uint64(1)
		for range dims {
			var d uint64
			read(&d)
			count *= d
		}
		var in info
		read(&in.typ)
		read(&in.offset)
		in.size = 2 * count
		infos = append(infos, in)
	}
	data := (uint64(pos) + align - 1) / align * align
	for _, in := range infos {
		var dst *[]byte
		switch in.typ {
		case 1:
			dst = &f16s
		case 30:
			dst = &bf16s
		default:
			continue
		}
		if len(*dst)+int(in.size) > limit {
			continue
		}
		b := make([]byte, in.size)
		if _, err := f.ReadAt(b, int64(data+in.offset)); err != nil {
			t.Fatal(err)
		}
		*dst = append(*dst, b...)
	}
	return f16s, bf16s
}

// readSafetensors returns up to limit bytes of the file's F16 and BF16
// tensors, each concatenated in file order.
func readSafetensors(t *testing.T, path string, limit int) (f16s, bf16s []byte) {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil || n > 100<<20 {
		t.Fatalf("%s: header length %d: %v", path, n, err)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatal(err)
	}
	var tensors map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &tensors); err != nil {
		t.Fatal(err)
	}
	type entry struct {
		DType   string   `json:"dtype"`
		Offsets [2]int64 `json:"data_offsets"`
	}
	var entries []entry
	for name, raw := range tensors {
		if name == "__metadata__" {
			continue
		}
		var e entry
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	// File order, so the sample is the same on every run.
	for i := range entries {
		for j := i + 1; j < len(entries); j++ {
			if entries[j].Offsets[0] < entries[i].Offsets[0] {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
	for _, e := range entries {
		var dst *[]byte
		switch e.DType {
		case "F16":
			dst = &f16s
		case "BF16":
			dst = &bf16s
		default:
			continue
		}
		size := e.Offsets[1] - e.Offsets[0]
		if len(*dst)+int(size) > limit {
			continue
		}
		b := make([]byte, size)
		if _, err := f.ReadAt(b, int64(8+n)+e.Offsets[0]); err != nil {
			t.Fatal(err)
		}
		*dst = append(*dst, b...)
	}
	return f16s, bf16s
}
