package videoenc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Fast start: a browser plays an MP4 while it downloads only when the
// movie box (moov) comes before the media data (mdat). Media Foundation's
// MP4 sink writes moov last. Its MF_MPEG4SINK_MOOV_BEFORE_MDAT option
// failed Finalize with E_ACCESSDENIED on the Sink Writer's own file stream
// and wrote a corrupt layout on a read-write stream (Windows 11 26200,
// 2026-10-02), so the provider moves moov itself, as qt-faststart does:
// moov goes before mdat and every chunk offset (stco, co64) grows by
// moov's size (ISO/IEC 14496-12 §8.7.5). When a 32-bit offset would pass
// 4 GiB, every stco becomes a co64 of the same offsets, which makes moov
// 4 bytes longer per chunk; the offsets then grow by that final size.

// maxMoov bounds the movie box read into memory: its sample tables grow
// with the frame count, a few kilobytes per hundred frames.
const maxMoov = 64 << 20

// topBox is one top-level box of a file: its type, where it starts, and its
// size with its header.
type topBox struct {
	kind         string
	offset, size int64
}

// topBoxes lists the top-level boxes of the file f of size length.
func topBoxes(f io.ReaderAt, length int64) ([]topBox, error) {
	var out []topBox
	var head [16]byte
	for offset := int64(0); offset < length; {
		if length-offset < 8 {
			return nil, fmt.Errorf("videoenc: %d bytes after the last box", length-offset)
		}
		if _, err := f.ReadAt(head[:8], offset); err != nil {
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(head[:4]))
		kind := string(head[4:8])
		switch size {
		case 0:
			size = length - offset
		case 1:
			if _, err := f.ReadAt(head[8:16], offset+8); err != nil {
				return nil, err
			}
			large := binary.BigEndian.Uint64(head[8:16])
			if large > math.MaxInt64 {
				return nil, fmt.Errorf("videoenc: box %q of %d bytes", kind, large)
			}
			size = int64(large)
		}
		if size < 8 || size > length-offset {
			return nil, fmt.Errorf("videoenc: box %q of %d bytes at %d in %d", kind, size, offset, length)
		}
		out = append(out, topBox{kind: kind, offset: offset, size: size})
		offset += size
	}
	return out, nil
}

// innerBox reads the box at the start of data, inside moov: its type, the
// length of its header and its size with the header.
func innerBox(data []byte) (kind string, header, size int, err error) {
	if len(data) < 8 {
		return "", 0, 0, errors.New("videoenc: a truncated box in moov")
	}
	size, header = int(binary.BigEndian.Uint32(data)), 8
	kind = string(data[4:8])
	if size == 1 {
		if len(data) < 16 {
			return "", 0, 0, errors.New("videoenc: a truncated box in moov")
		}
		large := binary.BigEndian.Uint64(data[8:16])
		if large > uint64(len(data)) {
			return "", 0, 0, fmt.Errorf("videoenc: box %q of %d bytes in moov", kind, large)
		}
		size, header = int(large), 16
	}
	if size == 0 {
		size = len(data)
	}
	if size < header || size > len(data) {
		return "", 0, 0, fmt.Errorf("videoenc: box %q of %d bytes in moov", kind, size)
	}
	return kind, header, size, nil
}

// sampleTablePath reports whether kind is a container on the way from moov
// to a chunk offset table.
func sampleTablePath(kind string) bool {
	switch kind {
	case "trak", "mdia", "minf", "stbl":
		return true
	}
	return false
}

// offsetTable checks the full-box header of an stco or co64 body whose
// entries are width bytes wide and returns its entry count.
func offsetTable(kind string, body []byte, width int) (int, error) {
	if len(body) < 8 {
		return 0, fmt.Errorf("videoenc: a short %s", kind)
	}
	n := int(binary.BigEndian.Uint32(body[4:]))
	if n < 0 || len(body) < 8+width*n {
		return 0, fmt.Errorf("videoenc: a short %s", kind)
	}
	return n, nil
}

// largestShortOffset is the largest 32-bit chunk offset (stco) in the boxes
// of data, or -1 when there is none.
func largestShortOffset(data []byte) (int64, error) {
	largest := int64(-1)
	for len(data) > 0 {
		kind, header, size, err := innerBox(data)
		if err != nil {
			return 0, err
		}
		body := data[header:size]
		switch {
		case sampleTablePath(kind):
			inner, err := largestShortOffset(body)
			if err != nil {
				return 0, err
			}
			largest = max(largest, inner)
		case kind == "stco":
			n, err := offsetTable(kind, body, 4)
			if err != nil {
				return 0, err
			}
			for i := 0; i < n; i++ {
				largest = max(largest, int64(binary.BigEndian.Uint32(body[8+4*i:])))
			}
		}
		data = data[size:]
	}
	return largest, nil
}

// rebuilt copies the boxes of data, each container on the way to a sample
// table with an 8-byte header sized to its new contents; with wide, every
// stco becomes a co64 of the same offsets. Every other box is copied as it
// is.
func rebuilt(data []byte, wide bool) ([]byte, error) {
	var out []byte
	for len(data) > 0 {
		kind, header, size, err := innerBox(data)
		if err != nil {
			return nil, err
		}
		body := data[header:size]
		switch {
		case sampleTablePath(kind):
			inner, err := rebuilt(body, wide)
			if err != nil {
				return nil, err
			}
			if out, err = appendBox(out, kind, inner); err != nil {
				return nil, err
			}
		case kind == "stco" && wide:
			n, err := offsetTable(kind, body, 4)
			if err != nil {
				return nil, err
			}
			table := append([]byte(nil), body[:8]...)
			for i := 0; i < n; i++ {
				table = binary.BigEndian.AppendUint64(table, uint64(binary.BigEndian.Uint32(body[8+4*i:])))
			}
			if out, err = appendBox(out, "co64", table); err != nil {
				return nil, err
			}
		default:
			out = append(out, data[:size]...)
		}
		data = data[size:]
	}
	return out, nil
}

// appendBox appends a box of kind holding body, with an 8-byte header.
func appendBox(out []byte, kind string, body []byte) ([]byte, error) {
	if uint64(8+len(body)) > math.MaxUint32 {
		return nil, fmt.Errorf("videoenc: a %s box of %d bytes", kind, 8+len(body))
	}
	out = binary.BigEndian.AppendUint32(out, uint32(8+len(body)))
	return append(append(out, kind...), body...), nil
}

// shiftChunkOffsets adds delta to every chunk offset in the boxes of data,
// descending through the containers that lead to a sample table.
func shiftChunkOffsets(data []byte, delta int64) error {
	for len(data) > 0 {
		kind, header, size, err := innerBox(data)
		if err != nil {
			return err
		}
		body := data[header:size]
		switch {
		case sampleTablePath(kind):
			if err := shiftChunkOffsets(body, delta); err != nil {
				return err
			}
		case kind == "stco":
			n, err := offsetTable(kind, body, 4)
			if err != nil {
				return err
			}
			for i := 0; i < n; i++ {
				at := body[8+4*i:]
				v := int64(binary.BigEndian.Uint32(at)) + delta
				if v > math.MaxUint32 {
					return errors.New("videoenc: a chunk offset passes 32 bits after moving moov")
				}
				binary.BigEndian.PutUint32(at, uint32(v))
			}
		case kind == "co64":
			n, err := offsetTable(kind, body, 8)
			if err != nil {
				return err
			}
			for i := 0; i < n; i++ {
				at := body[8+8*i:]
				binary.BigEndian.PutUint64(at, binary.BigEndian.Uint64(at)+uint64(delta))
			}
		}
		data = data[size:]
	}
	return nil
}

// movedMovie is the movie box movie rebuilt to stand before mdat: every
// chunk offset grows by the rebuilt box's own size, and every stco becomes
// a co64 when a 32-bit offset would otherwise pass 4 GiB.
func movedMovie(movie []byte) ([]byte, error) {
	_, header, size, err := innerBox(movie)
	if err != nil || size != len(movie) {
		return nil, errors.Join(errors.New("videoenc: a malformed moov"), err)
	}
	body := movie[header:]
	largest, err := largestShortOffset(body)
	if err != nil {
		return nil, err
	}
	inner, err := rebuilt(body, false)
	if err != nil {
		return nil, err
	}
	if largest >= 0 && largest+int64(8+len(inner)) > math.MaxUint32 {
		if inner, err = rebuilt(body, true); err != nil {
			return nil, err
		}
	}
	if err := shiftChunkOffsets(inner, int64(8+len(inner))); err != nil {
		return nil, err
	}
	return appendBox(nil, "moov", inner)
}

// fastStartPlan is what fast start writes: the file's top-level boxes and,
// when moov follows mdat, moov rebuilt for its place before mdat.
type fastStartPlan struct {
	boxes      []topBox
	length     int64
	moov, mdat int
	movie      []byte
}

// planFastStart reads the top-level boxes of src, of size length, and
// rebuilds its moov when it follows mdat.
func planFastStart(src io.ReaderAt, length int64) (*fastStartPlan, error) {
	boxes, err := topBoxes(src, length)
	if err != nil {
		return nil, err
	}
	p := &fastStartPlan{boxes: boxes, length: length, moov: -1, mdat: -1}
	for i, b := range boxes {
		switch b.kind {
		case "moov":
			if p.moov >= 0 {
				return nil, errors.New("videoenc: two moov boxes")
			}
			p.moov = i
		case "mdat":
			if p.mdat >= 0 {
				return nil, errors.New("videoenc: two mdat boxes")
			}
			p.mdat = i
		}
	}
	if p.moov < 0 || p.mdat < 0 {
		return nil, errors.New("videoenc: no moov or no mdat")
	}
	if p.moov < p.mdat {
		return p, nil
	}
	m := boxes[p.moov]
	if m.size > maxMoov {
		return nil, fmt.Errorf("videoenc: a moov of %d bytes", m.size)
	}
	movie := make([]byte, m.size)
	if _, err := src.ReadAt(movie, m.offset); err != nil {
		return nil, err
	}
	if p.movie, err = movedMovie(movie); err != nil {
		return nil, err
	}
	return p, nil
}

// write writes src to dst as the plan says: unchanged when moov already
// leads, else the boxes before mdat, the rebuilt moov, then the rest
// without the old moov.
func (p *fastStartPlan) write(dst io.Writer, src io.ReaderAt) error {
	copyRange := func(from, to int64) error {
		_, err := io.Copy(dst, io.NewSectionReader(src, from, to-from))
		return err
	}
	if p.movie == nil {
		return copyRange(0, p.length)
	}
	start, m := p.boxes[p.mdat].offset, p.boxes[p.moov]
	if err := copyRange(0, start); err != nil {
		return err
	}
	if _, err := dst.Write(p.movie); err != nil {
		return err
	}
	if err := copyRange(start, m.offset); err != nil {
		return err
	}
	return copyRange(m.offset+m.size, p.length)
}

// fastStart writes the MP4 at in to out with moov before mdat and every
// chunk offset moved with mdat. A file whose moov already leads is copied
// as it is. Nothing is written to out when in cannot be read as an MP4.
func fastStart(in, out string) error {
	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	plan, err := planFastStart(src, info.Size())
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	return errors.Join(plan.write(dst, src), dst.Close())
}
