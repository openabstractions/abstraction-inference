package modelfiles

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// decodedSource is a file whose bytes are the object itself, read by a
// decoder that copies them: FillDecoded's proof is tested apart from any
// pack format.
func decodedSource(t *testing.T) (Extent, []byte) {
	t.Helper()
	data := payload()
	path := filepath.Join(t.TempDir(), "object.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	n := int64(len(data))
	return Extent{Path: path, Packed: n, Length: n, Method: zip.Store}, data
}

func copyDecoder(data []byte, seen *[][]string) Decoder {
	return func(_ context.Context, view []byte, spans []string) (func(int) func([]byte), error) {
		*seen = append(*seen, spans)
		copy(view, data)
		return nil, nil
	}
}

// INF-MS8: a decoder that leaves bytes for the hash pass gets each span's
// blocks in order, each once, and completes them before the fill hashes
// them; the fill proves and records the completed bytes. Bytes it leaves
// wrong are refused.
func TestFillDecodedFinishesBlocksBeforeHashing(t *testing.T) {
	src, data := decodedSource(t)
	r, err := OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []int64 // each block's offset in the object, as handed over
	var spans []int
	flipped := func(skip int64) Decoder {
		return func(_ context.Context, view []byte, _ []string) (func(int) func([]byte), error) {
			for i := range view {
				view[i] = data[i] ^ 0x5a
			}
			return func(span int) func([]byte) {
				mu.Lock()
				spans = append(spans, span)
				mu.Unlock()
				at := int64(span) * ChunkSize
				return func(block []byte) {
					mu.Lock()
					got = append(got, at)
					mu.Unlock()
					for i := range block {
						if at+int64(i) != skip {
							block[i] ^= 0x5a
						}
					}
					at += int64(len(block))
				}
			}, nil
		}
	}
	s, err := r.FillDecoded(context.Background(), src, digestOf(data), flipped(-1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mapped(t, s), data) {
		t.Fatal("the section holds other bytes than the completed ones")
	}
	s.Close()
	if _, ok := r.Lookup(digestOf(data)); !ok {
		t.Fatal("the completed fill left no record")
	}
	slices.Sort(spans)
	slices.Sort(got)
	var want []int64
	for at := int64(0); at < src.Length; at += hashBlock {
		want = append(want, at)
	}
	if !slices.Equal(spans, []int{0, 1, 2}[:chunkCount(src.Length)]) || !slices.Equal(got, want) {
		t.Fatalf("spans %v, blocks at %v, want %v", spans, got, want)
	}
	var n counter
	if _, err := r.FillDecoded(context.Background(), src, digestOf(data), flipped(ChunkSize+5), n.release); !errors.Is(err, ErrDigest) || n.n.Load() != 1 {
		t.Fatalf("a block left incomplete: %v, release %d", err, n.n.Load())
	}
}

// INF-MS8: a first fill hashes the whole object and records its spans; the
// next fill hands the decoder the record and trusts it.
func TestFillDecodedRecordsThenTrusts(t *testing.T) {
	src, data := decodedSource(t)
	r, err := OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	s, err := r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := s.ChunkDigests()
	if s.RecordErr() != nil {
		t.Fatalf("a recorded fill reports %v", s.RecordErr())
	}
	s.Close()
	rec, ok := r.Lookup(digestOf(data))
	if !ok || !slices.Equal(rec.Spans, want) || len(seen) != 1 || seen[0] != nil {
		t.Fatalf("first fill: record %v (%v), decoder saw %v", rec.Spans, ok, seen)
	}
	s, err = r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Equal(seen[1], want) || !slices.Equal(s.ChunkDigests(), want) {
		t.Fatalf("second fill: decoder saw %v, want the record", seen[1])
	}
}

// INF-MS8: the fill hashes the spans itself. A decoder that ignores the
// record's spans and writes other bytes is refused by the fill.
func TestFillDecodedChecksTheRecordItself(t *testing.T) {
	src, data := decodedSource(t)
	r, err := OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	s, err := r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	other := append([]byte(nil), data...)
	other[ChunkSize+7] ^= 1 // inside the second span
	var n counter
	_, err = r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(other, &seen), n.release)
	if !errors.Is(err, ErrDigest) || n.n.Load() != 1 {
		t.Fatalf("decoder writing other bytes under a record: %v, release %d", err, n.n.Load())
	}
}

// INF-MS8: a proven first fill whose record cannot be written returns its
// section with no error; RecordErr wraps ErrRecord, and no record exists
// afterwards. A fill that wrote its record has no RecordErr.
func TestFillDecodedKeepsAProvenSectionWhenTheRecordFails(t *testing.T) {
	src, data := decodedSource(t)
	dir := filepath.Join(t.TempDir(), "records")
	r, err := OpenSpanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	var n counter
	s, err := r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), n.release)
	if err != nil || s == nil || !errors.Is(s.RecordErr(), ErrRecord) {
		t.Fatalf("record folder gone: section %v, %v, record error %v", s, err, s.RecordErr())
	}
	if s.Digest() != digestOf(data) || s.Freed() || n.n.Load() != 0 {
		t.Fatalf("the kept section: digest %s, freed %v, release %d", s.Digest(), s.Freed(), n.n.Load())
	}
	// mapped's loan is the section's last; its end frees it (INF-MS5).
	if !bytes.Equal(mapped(t, s), data) {
		t.Fatal("the kept section holds other bytes")
	}
	s.Close()
	if n.n.Load() != 1 {
		t.Fatalf("release called %d times", n.n.Load())
	}
	if _, ok := r.Lookup(digestOf(data)); ok {
		t.Fatal("a record exists")
	}
}

// INF-MS8: a decoded fill needs the record store.
func TestFillDecodedNeedsRecords(t *testing.T) {
	src, data := decodedSource(t)
	var seen [][]string
	var n counter
	var r *SpanRecords
	if _, err := r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), n.release); err == nil || n.n.Load() != 1 || len(seen) != 0 {
		t.Fatalf("no records: %v, release %d, decoder ran %d times", err, n.n.Load(), len(seen))
	}
}

// INF-MS8: a region reader gives any range of the region, at any offset
// and length, through its unbuffered handle.
func TestRegionReaderReadsUnalignedRanges(t *testing.T) {
	p := writePack(t)
	src := p.unaligned // a STORED member off every sector boundary
	r, err := OpenRegion(src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, c := range [][2]int64{{0, 1}, {1, 4095}, {4095, 2}, {12345, 70000}, {src.Packed - 3, 3}, {0, src.Packed}} {
		buf := make([]byte, c[1])
		if n, err := r.ReadAt(buf, c[0]); err != nil || int64(n) != c[1] || !bytes.Equal(buf, p.data[c[0]:c[0]+c[1]]) {
			t.Fatalf("range %v: %d bytes, %v", c, n, err)
		}
	}
	if _, err := r.ReadAt(make([]byte, 2), src.Packed-1); !errors.Is(err, ErrSource) {
		t.Fatalf("a range past the region: %v", err)
	}
}

// INF-MS8: bytes of another digest on a first fill, a decoder's error and a
// record of another length free the section and end the holding once.
func TestFillDecodedRefusals(t *testing.T) {
	src, data := decodedSource(t)
	r, err := OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	other := append([]byte(nil), data...)
	other[len(other)/2] ^= 1
	boom := errors.New("decoder failed")
	cases := map[string]struct {
		decode Decoder
		want   error
	}{
		"other bytes": {copyDecoder(other, &seen), ErrDigest},
		"decoder":     {func(context.Context, []byte, []string) (func(int) func([]byte), error) { return nil, boom }, boom},
	}
	for name, c := range cases {
		var n counter
		if _, err := r.FillDecoded(context.Background(), src, digestOf(data), c.decode, n.release); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		if n.n.Load() != 1 {
			t.Errorf("%s: release called %d times", name, n.n.Load())
		}
	}
	if _, ok := r.Lookup(digestOf(data)); ok {
		t.Fatal("a refused fill left a record")
	}
	rec := SpanRecord{Digest: digestOf(data), Length: src.Length + ChunkSize, Spans: make([]string, chunkCount(src.Length+ChunkSize))}
	for i := range rec.Spans {
		rec.Spans[i] = digestOf(nil)[7:]
	}
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}
	var n counter
	if _, err := r.FillDecoded(context.Background(), src, digestOf(data), copyDecoder(data, &seen), n.release); !errors.Is(err, ErrDigest) || n.n.Load() != 1 {
		t.Fatalf("record of another length: %v, release %d", err, n.n.Load())
	}
}
