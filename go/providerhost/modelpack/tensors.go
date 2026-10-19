package modelpack

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ErrNotWeightFile: a file is neither a safetensors nor a GGUF file, or its header
// contradicts itself or the file's size.
var ErrNotWeightFile = errors.New("modelpack: not a well-formed safetensors or GGUF file")

const (
	// maxSafetensorsHeader bounds a safetensors JSON header.
	maxSafetensorsHeader = 100 << 20
	// maxGGUFEntries bounds a GGUF file's tensor and key-value counts.
	maxGGUFEntries = 1 << 20
	// maxGGUFString bounds one GGUF string.
	maxGGUFString = 1 << 24
	// maxGGUFArray bounds one GGUF array's element count.
	maxGGUFArray = 1 << 28
)

func formatError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotWeightFile, fmt.Sprintf(format, args...))
}

// ReadTensors reads the header of a weight file of size bytes, a
// safetensors or a GGUF file told apart by its first bytes, and returns its
// format ("safetensors" or "gguf") and its tensors in file order, each with
// its byte range and dtype as the file names it. A GGUF file's tensors are
// listed when their size follows from their type: F32, F16 and BF16; the
// bytes of every other tensor are left to Pack's stored runs. Tensor data is
// never read.
func ReadTensors(r io.ReaderAt, size int64) (string, []Tensor, error) {
	var head [8]byte
	if size < 8 {
		return "", nil, formatError("%d bytes", size)
	}
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return "", nil, err
	}
	if string(head[:4]) == "GGUF" {
		t, err := ggufTensors(r, size)
		return "gguf", t, err
	}
	t, err := safetensorsTensors(r, size, binary.LittleEndian.Uint64(head[:]))
	return "safetensors", t, err
}

func safetensorsTensors(r io.ReaderAt, size int64, n uint64) ([]Tensor, error) {
	if n < 2 || n > maxSafetensorsHeader || int64(n) > size-8 {
		return nil, formatError("safetensors header of %d bytes in a file of %d", n, size)
	}
	header := make([]byte, n)
	if _, err := r.ReadAt(header, 8); err != nil {
		return nil, err
	}
	if header[0] != '{' {
		return nil, formatError("no safetensors JSON header")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(header, &entries); err != nil {
		return nil, formatError("safetensors header: %v", err)
	}
	data := 8 + int64(n)
	var out []Tensor
	for name, raw := range entries {
		if name == "__metadata__" {
			continue
		}
		var e struct {
			DType   string   `json:"dtype"`
			Shape   []int64  `json:"shape"`
			Offsets [2]int64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, formatError("tensor %q: %v", name, err)
		}
		begin, end := e.Offsets[0], e.Offsets[1]
		if begin < 0 || end < begin || end > size-data || e.DType == "" {
			return nil, formatError("tensor %q at [%d, %d) of %d data bytes", name, begin, end, size-data)
		}
		if end > begin {
			out = append(out, Tensor{Offset: data + begin, Length: end - begin, DType: e.DType, Name: name, Shape: e.Shape})
		}
	}
	return ordered(out)
}

// ordered sorts tensors by offset and refuses two that overlap.
func ordered(t []Tensor) ([]Tensor, error) {
	slices.SortFunc(t, func(a, b Tensor) int { return cmp.Compare(a.Offset, b.Offset) })
	for i := 1; i < len(t); i++ {
		if t[i].Offset < t[i-1].Offset+t[i-1].Length {
			return nil, formatError("tensors at %d and %d overlap", t[i-1].Offset, t[i].Offset)
		}
	}
	return t, nil
}

// ggufReader reads a GGUF header forward, counting its position.
type ggufReader struct {
	r   *bufio.Reader
	pos int64
	err error
}

func (g *ggufReader) bytes(n int64) []byte {
	if g.err != nil {
		return nil
	}
	b := make([]byte, n)
	k, err := io.ReadFull(g.r, b)
	g.pos += int64(k)
	if err != nil {
		g.err = formatError("GGUF header ends early: %v", err)
	}
	return b
}

func (g *ggufReader) skip(n int64) {
	if g.err != nil {
		return
	}
	k, err := g.r.Discard(int(n))
	g.pos += int64(k)
	if err != nil {
		g.err = formatError("GGUF header ends early: %v", err)
	}
}

func (g *ggufReader) u32() uint32 {
	if b := g.bytes(4); g.err == nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (g *ggufReader) u64() uint64 {
	if b := g.bytes(8); g.err == nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (g *ggufReader) str() string {
	n := g.u64()
	if g.err == nil && n > maxGGUFString {
		g.err = formatError("GGUF string of %d bytes", n)
	}
	return string(g.bytes(int64(n)))
}

// ggufScalar is the byte size of each fixed-width GGUF value type.
var ggufScalar = map[uint32]int64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// value reads past one value of type typ and returns it when it is an
// unsigned integer (for general.alignment).
func (g *ggufReader) value(typ uint32, depth int) uint64 {
	switch typ {
	case 8:
		g.str()
	case 9:
		et, n := g.u32(), g.u64()
		if g.err == nil && (n > maxGGUFArray || depth > 2) {
			g.err = formatError("GGUF array of %d elements", n)
		}
		if w, ok := ggufScalar[et]; ok && g.err == nil {
			g.skip(int64(n) * w)
			return 0
		}
		for i := uint64(0); i < n && g.err == nil; i++ {
			g.value(et, depth+1)
		}
	default:
		w, ok := ggufScalar[typ]
		if !ok {
			if g.err == nil {
				g.err = formatError("GGUF value type %d", typ)
			}
			return 0
		}
		b := g.bytes(w)
		if g.err == nil {
			switch w {
			case 4:
				return uint64(binary.LittleEndian.Uint32(b))
			case 8:
				return binary.LittleEndian.Uint64(b)
			}
		}
	}
	return 0
}

// ggufTypes are the ggml types whose size follows from their element count.
var ggufTypes = map[uint32]struct {
	name  string
	width int64
}{0: {"F32", 4}, 1: {"F16", 2}, 30: {"BF16", 2}}

func ggufTensors(r io.ReaderAt, size int64) ([]Tensor, error) {
	g := &ggufReader{r: bufio.NewReaderSize(io.NewSectionReader(r, 0, size), 1<<20)}
	g.skip(4)
	if v := g.u32(); g.err == nil && v != 2 && v != 3 {
		return nil, formatError("GGUF version %d", v)
	}
	tensors, kvs := g.u64(), g.u64()
	if g.err == nil && (tensors > maxGGUFEntries || kvs > maxGGUFEntries) {
		return nil, formatError("GGUF header of %d tensors and %d keys", tensors, kvs)
	}
	align := uint64(32)
	for range kvs {
		key := g.str()
		typ := g.u32()
		if v := g.value(typ, 0); key == "general.alignment" && g.err == nil {
			align = v
		}
		if g.err != nil {
			return nil, g.err
		}
	}
	if align == 0 || align&(align-1) != 0 || align > 1<<20 {
		return nil, formatError("GGUF alignment %d", align)
	}
	type info struct {
		name         string
		shape        []int64
		typ          uint32
		offset, size uint64
		known        bool
	}
	infos := make([]info, 0, min(tensors, 1<<16))
	for range tensors {
		var in info
		in.name = g.str()
		dims := g.u32()
		if g.err == nil && (dims == 0 || dims > 8) {
			return nil, formatError("GGUF tensor of %d dimensions", dims)
		}
		count := uint64(1)
		for range dims {
			d := g.u64()
			if d != 0 && count > uint64(size)/d {
				return nil, formatError("GGUF tensor larger than the file")
			}
			count *= d
			in.shape = append(in.shape, int64(d))
		}
		in.typ, in.offset = g.u32(), g.u64()
		if t, ok := ggufTypes[in.typ]; ok {
			in.size, in.known = count*uint64(t.width), true
		}
		if g.err != nil {
			return nil, g.err
		}
		infos = append(infos, in)
	}
	data := (uint64(g.pos) + align - 1) / align * align
	var out []Tensor
	for _, in := range infos {
		if !in.known || in.size == 0 {
			continue
		}
		if data+in.offset < data || in.offset > uint64(size) || data+in.offset+in.size > uint64(size) {
			return nil, formatError("GGUF tensor at %d of %d bytes lies beyond the file", in.offset, in.size)
		}
		out = append(out, Tensor{Offset: int64(data + in.offset), Length: int64(in.size), DType: ggufTypes[in.typ].name,
			Name: in.name, Shape: in.shape})
	}
	return ordered(out)
}
