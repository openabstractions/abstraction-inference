package videoenc

import (
	"encoding/binary"
	"fmt"
	"io"
	"testing"
)

// sparseFile is a file of length bytes, zero but for its parts, read
// without being held in memory.
type sparseFile struct {
	length int64
	parts  []sparsePart
}

type sparsePart struct {
	at   int64
	data []byte
}

func (f *sparseFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.length {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), f.length-off))
	clear(p[:n])
	for _, part := range f.parts {
		from, to := max(off, part.at), min(off+int64(n), part.at+int64(len(part.data)))
		if from < to {
			copy(p[from-off:to-off], part.data[from-part.at:to-part.at])
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// endsWriter keeps the first headBytes bytes written to it and every byte
// from tailAt on, and counts the rest.
type endsWriter struct {
	n          int64
	head, tail []byte
	headBytes  int
	tailAt     int64
}

func (w *endsWriter) Write(p []byte) (int, error) {
	start := w.n
	w.n += int64(len(p))
	if keep := w.headBytes - len(w.head); keep > 0 {
		w.head = append(w.head, p[:min(keep, len(p))]...)
	}
	if w.n > w.tailAt {
		from := max(start, w.tailAt)
		w.tail = append(w.tail, p[from-start:]...)
	}
	return len(p), nil
}

// at is the four bytes written at offset, which lies in the kept tail.
func (w *endsWriter) at(offset uint64) string {
	i := int64(offset) - w.tailAt
	if i < 0 || i+4 > int64(len(w.tail)) {
		return fmt.Sprintf("<offset %#x outside the kept tail>", offset)
	}
	return string(w.tail[i : i+4])
}

// stcoBox is an stco of offsets.
func stcoBox(offsets ...uint32) []byte {
	body := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 0}, uint32(len(offsets)))
	for _, o := range offsets {
		body = binary.BigEndian.AppendUint32(body, o)
	}
	return mkbox("stco", body)
}

// INF-VE1, review of 0410d167f: chunk offsets near 0xFFFFF000 that would
// pass 4 GiB once moov moves before mdat turn every stco into a co64,
// moov grows by 4 bytes per chunk, and every offset then names its chunk's
// own bytes in the written file. The file is about 4 GiB, read and written
// without being held: zero but for its boxes.
func TestFastStartWidensOffsetsPastFourGiB(t *testing.T) {
	const dataAt = 0xFFFFF000
	const chunk = 0x400
	ftyp := mkbox("ftyp", []byte("isom\x00\x00\x02\x00"))
	mdatAt := int64(dataAt - 8)
	free := binary.BigEndian.AppendUint32(nil, uint32(mdatAt-int64(len(ftyp))))
	free = append(free, "free"...)
	// mdat: four chunks, each starting with its own marker.
	var media []byte
	for i := 0; i < 4; i++ {
		c := make([]byte, chunk)
		copy(c, fmt.Sprintf("CHK%d", i))
		media = append(media, c...)
	}
	mdat := append(binary.BigEndian.AppendUint32(nil, uint32(8+len(media))), "mdat"...)
	mdat = append(mdat, media...)
	// Two tracks: one with three chunks, one with the last chunk alone. A
	// user-data box of 8 KiB makes moov large enough that every offset
	// passes 32 bits once moved.
	trak := func(stco []byte) []byte {
		return mkbox("trak", mkbox("tkhd", make([]byte, 84)), mkbox("mdia", mkbox("minf", mkbox("stbl", mkbox("stsd", make([]byte, 16)), stco))))
	}
	moov := mkbox("moov", mkbox("mvhd", make([]byte, 100)),
		trak(stcoBox(dataAt, dataAt+chunk, dataAt+2*chunk)),
		trak(stcoBox(dataAt+3*chunk)),
		mkbox("udta", make([]byte, 8192)))
	moovAt := mdatAt + int64(len(mdat))
	src := &sparseFile{length: moovAt + int64(len(moov)), parts: []sparsePart{
		{0, append(append([]byte(nil), ftyp...), free...)},
		{mdatAt, mdat},
		{moovAt, moov},
	}}

	plan, err := planFastStart(src, src.length)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(moov) + 4*4; len(plan.movie) != want {
		t.Fatalf("the moved moov is %d bytes, want %d (4 more per chunk)", len(plan.movie), want)
	}
	out := &endsWriter{headBytes: 1 << 16, tailAt: dataAt - 0x1000}
	if err := plan.write(out, src); err != nil {
		t.Fatal(err)
	}
	if out.n != src.length+16 {
		t.Fatalf("wrote %d bytes, want %d", out.n, src.length+16)
	}
	// The file reads ftyp, free, moov, mdat: moov takes mdat's old place,
	// which lies in the kept tail, and mdat follows it.
	if string(out.head[:len(ftyp)]) != string(ftyp) || string(out.head[len(ftyp)+4:len(ftyp)+8]) != "free" {
		t.Fatalf("the file does not start with ftyp and free: %q", out.head[:len(ftyp)+8])
	}
	at := int(mdatAt - out.tailAt)
	top, err := boxes(out.tail[at : at+len(plan.movie)])
	next := string(out.tail[at+len(plan.movie)+4 : at+len(plan.movie)+8])
	if err != nil || len(top) != 1 || top[0].kind != "moov" || next != "mdat" {
		t.Fatalf("boxes at mdat's old place %v then %q (%v)", top, next, err)
	}
	inner, err := boxes(top[0].body)
	if err != nil {
		t.Fatal(err)
	}
	marker := 0
	for _, b := range inner {
		if b.kind != "trak" {
			continue
		}
		mdia, _ := boxes(b.body)
		minf, _ := boxes(child(mdia, "mdia"))
		stbl, _ := boxes(child(minf, "minf"))
		table, _ := boxes(child(stbl, "stbl"))
		if child(table, "stco") != nil || child(table, "co64") == nil {
			t.Fatalf("a track's sample table %v keeps an stco or has no co64", table)
		}
		co64 := child(table, "co64")
		n := int(binary.BigEndian.Uint32(co64[4:]))
		for i := 0; i < n; i++ {
			offset := binary.BigEndian.Uint64(co64[8+8*i:])
			if offset <= 0xFFFFFFFF {
				t.Fatalf("chunk %d at %#x did not pass 32 bits", marker, offset)
			}
			if got, want := out.at(offset), fmt.Sprintf("CHK%d", marker); got != want {
				t.Fatalf("chunk %d at %#x reads %q, want %q", marker, offset, got, want)
			}
			marker++
		}
	}
	if marker != 4 {
		t.Fatalf("%d chunk offsets, want 4", marker)
	}

	// A moov whose moved offsets stay inside 32 bits keeps its stco.
	small, err := movedMovie(mkbox("moov", trak(stcoBox(64, 64+chunk))))
	if err != nil {
		t.Fatal(err)
	}
	moved, _ := boxes(small)
	traks, _ := boxes(moved[0].body)
	mdia, _ := boxes(traks[0].body)
	minf, _ := boxes(child(mdia, "mdia"))
	stbl, _ := boxes(child(minf, "minf"))
	table, _ := boxes(child(stbl, "stbl"))
	stco := child(table, "stco")
	if stco == nil || binary.BigEndian.Uint32(stco[8:]) != uint32(64+len(small)) {
		t.Fatalf("a small moov: %v", table)
	}
}
