package modelpack

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// zipWriter writes the zip subset of an OA model archive (INF-MP6): STORED
// entries whose sizes and CRC-32 are known before their data, each data
// start on a 64 KiB boundary when asked, padded through the zipalign extra
// field (ID 0xD935) in the local header alone, and Zip64 fields wherever a
// size, an offset or the entry count needs them (APPNOTE.TXT 4.3.9, 4.5.3,
// 4.3.14). Go's archive/zip repeats a local extra field in the central
// directory and writes no Zip64 local field for a raw entry, so the archive
// writes its own headers.
type zipWriter struct {
	w       io.Writer
	n       int64
	entries []zipEntry
	// forceZip64 writes Zip64 fields for every size and offset; a test sets
	// it to read Zip64 archives without writing 4 GiB.
	forceZip64 bool
}

type zipEntry struct {
	name         string
	crc          uint32
	size, offset int64
}

const (
	zipVersion45   = 45     // Zip64
	zipVersion20   = 20     // STORED, directories
	zipFlagUTF8    = 0x0800 // the name is UTF-8 (APPNOTE.TXT 4.4.4, bit 11)
	zipDOSDate     = 0x0021 // 1980-01-01, the earliest DOS date
	zipZip64ID     = 0x0001
	zipAlignID     = 0xd935
	zip32Max       = math.MaxUint32
	zip16Max       = math.MaxUint16
	zipLocalLen    = 30
	zipCentralLen  = 46
	zipEOCDLen     = 22
	zip64EOCDLen   = 56
	zip64LocatorLn = 20
)

func (z *zipWriter) Write(p []byte) (int, error) {
	n, err := z.w.Write(p)
	z.n += int64(n)
	return n, err
}

// begin writes the local header of a STORED entry of size bytes with the
// given CRC-32; its data follows through Write. With align its data starts
// on a 64 KiB boundary.
func (z *zipWriter) begin(name string, size int64, crc uint32, align bool) error {
	if len(name) == 0 || len(name) > zip16Max {
		return fmt.Errorf("modelpack: zip entry name of %d bytes", len(name))
	}
	zip64 := z.forceZip64 || size >= zip32Max
	var extra []byte
	if zip64 {
		extra = binary.LittleEndian.AppendUint16(extra, zipZip64ID)
		extra = binary.LittleEndian.AppendUint16(extra, 16)
		extra = binary.LittleEndian.AppendUint64(extra, uint64(size))
		extra = binary.LittleEndian.AppendUint64(extra, uint64(size))
	}
	if align {
		fixed := int64(len(extra)) + 6
		at := z.n + zipLocalLen + int64(len(name)) + fixed
		pad := (packAlign - at%packAlign) % packAlign
		// The extra field's length is 16 bits. When the padding would carry
		// it past 65,535 bytes, a gap of at most `fixed` zero bytes before the
		// local header takes the excess; zip readers find each local header
		// through the central directory.
		if over := fixed + pad - zip16Max; over > 0 {
			if _, err := z.Write(make([]byte, over)); err != nil {
				return err
			}
			pad -= over
		}
		extra = binary.LittleEndian.AppendUint16(extra, zipAlignID)
		extra = binary.LittleEndian.AppendUint16(extra, uint16(2+pad))
		// zipalign's 16-bit alignment field cannot hold 64 KiB; 0 marks the
		// field as padding alone.
		extra = binary.LittleEndian.AppendUint16(extra, 0)
		extra = append(extra, make([]byte, pad)...)
	}
	z.entries = append(z.entries, zipEntry{name: name, crc: crc, size: size, offset: z.n})
	h := make([]byte, 0, zipLocalLen+len(name)+len(extra))
	h = binary.LittleEndian.AppendUint32(h, 0x04034b50)
	h = binary.LittleEndian.AppendUint16(h, version(zip64))
	h = binary.LittleEndian.AppendUint16(h, zipFlagUTF8)
	h = binary.LittleEndian.AppendUint16(h, 0) // STORED
	h = binary.LittleEndian.AppendUint16(h, 0) // time
	h = binary.LittleEndian.AppendUint16(h, zipDOSDate)
	h = binary.LittleEndian.AppendUint32(h, crc)
	h = binary.LittleEndian.AppendUint32(h, size32(size, zip64))
	h = binary.LittleEndian.AppendUint32(h, size32(size, zip64))
	h = binary.LittleEndian.AppendUint16(h, uint16(len(name)))
	h = binary.LittleEndian.AppendUint16(h, uint16(len(extra)))
	h = append(h, name...)
	h = append(h, extra...)
	_, err := z.Write(h)
	return err
}

func version(zip64 bool) uint16 {
	if zip64 {
		return zipVersion45
	}
	return zipVersion20
}

func size32(v int64, zip64 bool) uint32 {
	if zip64 {
		return zip32Max
	}
	return uint32(v)
}

// close writes the central directory and its end records.
func (z *zipWriter) close() error {
	start := z.n
	for _, e := range z.entries {
		sizes := z.forceZip64 || e.size >= zip32Max
		offset := z.forceZip64 || e.offset >= zip32Max
		var extra []byte
		if sizes || offset {
			var body []byte
			if sizes {
				body = binary.LittleEndian.AppendUint64(body, uint64(e.size))
				body = binary.LittleEndian.AppendUint64(body, uint64(e.size))
			}
			if offset {
				body = binary.LittleEndian.AppendUint64(body, uint64(e.offset))
			}
			extra = binary.LittleEndian.AppendUint16(extra, zipZip64ID)
			extra = binary.LittleEndian.AppendUint16(extra, uint16(len(body)))
			extra = append(extra, body...)
		}
		zip64 := sizes || offset
		h := make([]byte, 0, zipCentralLen+len(e.name)+len(extra))
		h = binary.LittleEndian.AppendUint32(h, 0x02014b50)
		h = binary.LittleEndian.AppendUint16(h, version(zip64))
		h = binary.LittleEndian.AppendUint16(h, version(zip64))
		h = binary.LittleEndian.AppendUint16(h, zipFlagUTF8)
		h = binary.LittleEndian.AppendUint16(h, 0)
		h = binary.LittleEndian.AppendUint16(h, 0)
		h = binary.LittleEndian.AppendUint16(h, zipDOSDate)
		h = binary.LittleEndian.AppendUint32(h, e.crc)
		h = binary.LittleEndian.AppendUint32(h, size32(e.size, sizes))
		h = binary.LittleEndian.AppendUint32(h, size32(e.size, sizes))
		h = binary.LittleEndian.AppendUint16(h, uint16(len(e.name)))
		h = binary.LittleEndian.AppendUint16(h, uint16(len(extra)))
		h = binary.LittleEndian.AppendUint16(h, 0) // comment
		h = binary.LittleEndian.AppendUint16(h, 0) // disk
		h = binary.LittleEndian.AppendUint16(h, 0) // internal attributes
		h = binary.LittleEndian.AppendUint32(h, 0) // external attributes
		h = binary.LittleEndian.AppendUint32(h, size32(e.offset, offset))
		h = append(h, e.name...)
		h = append(h, extra...)
		if _, err := z.Write(h); err != nil {
			return err
		}
	}
	size := z.n - start
	count := int64(len(z.entries))
	zip64 := z.forceZip64 || count >= zip16Max || size >= zip32Max || start >= zip32Max
	if zip64 {
		at := z.n
		r := make([]byte, 0, zip64EOCDLen+zip64LocatorLn)
		r = binary.LittleEndian.AppendUint32(r, 0x06064b50)
		r = binary.LittleEndian.AppendUint64(r, zip64EOCDLen-12)
		r = binary.LittleEndian.AppendUint16(r, zipVersion45)
		r = binary.LittleEndian.AppendUint16(r, zipVersion45)
		r = binary.LittleEndian.AppendUint32(r, 0)
		r = binary.LittleEndian.AppendUint32(r, 0)
		r = binary.LittleEndian.AppendUint64(r, uint64(count))
		r = binary.LittleEndian.AppendUint64(r, uint64(count))
		r = binary.LittleEndian.AppendUint64(r, uint64(size))
		r = binary.LittleEndian.AppendUint64(r, uint64(start))
		r = binary.LittleEndian.AppendUint32(r, 0x07064b50)
		r = binary.LittleEndian.AppendUint32(r, 0)
		r = binary.LittleEndian.AppendUint64(r, uint64(at))
		r = binary.LittleEndian.AppendUint32(r, 1)
		if _, err := z.Write(r); err != nil {
			return err
		}
	}
	e := make([]byte, 0, zipEOCDLen)
	e = binary.LittleEndian.AppendUint32(e, 0x06054b50)
	e = binary.LittleEndian.AppendUint16(e, 0)
	e = binary.LittleEndian.AppendUint16(e, 0)
	n16 := uint16(min(count, zip16Max))
	if zip64 {
		n16 = zip16Max
	}
	e = binary.LittleEndian.AppendUint16(e, n16)
	e = binary.LittleEndian.AppendUint16(e, n16)
	e = binary.LittleEndian.AppendUint32(e, size32(size, zip64))
	e = binary.LittleEndian.AppendUint32(e, size32(start, zip64))
	e = binary.LittleEndian.AppendUint16(e, 0)
	_, err := z.Write(e)
	return err
}
