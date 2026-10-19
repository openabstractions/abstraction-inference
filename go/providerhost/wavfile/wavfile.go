// Package wavfile reads and writes the one WAV shape providers hand to speech
// engines and return from them: RIFF/WAVE with format tag 1 (PCM), 16-bit
// little-endian samples, mono or stereo, a 16-byte fmt chunk and one data
// chunk behind the 44-byte canonical header. The whisper.cpp and Kokoros
// policy packages share it (inference CONTRACT.md INF-WC3, INF-KO3).
package wavfile

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrFormat reports bytes that are not a WAV file this package accepts.
var ErrFormat = errors.New("wav: not 16-bit PCM WAV")

// HeaderBytes is the size of the canonical header Header writes.
const HeaderBytes = 44

// Sample rates Decode accepts, inclusive. whisper-server resamples any of them
// to 16 kHz; Kokoros produces 24 kHz.
const (
	MinRate = 8000
	MaxRate = 48000
)

// Format is the shape of the samples: 16-bit PCM at Rate with Channels.
type Format struct {
	Channels uint16
	Rate     uint32
}

func (f Format) blockAlign() int { return int(f.Channels) * 2 }

func (f Format) valid() bool {
	return (f.Channels == 1 || f.Channels == 2) && f.Rate >= MinRate && f.Rate <= MaxRate
}

// Header returns the 44-byte canonical header for dataBytes bytes of 16-bit
// PCM in format f. It refuses a format Decode would refuse, a data length that
// is not whole sample frames, and a file whose RIFF size would not fit 32
// bits.
func Header(f Format, dataBytes int) ([]byte, error) {
	if !f.valid() || dataBytes < 0 || dataBytes%f.blockAlign() != 0 || int64(dataBytes) > int64(^uint32(0))-(HeaderBytes-8) {
		return nil, fmt.Errorf("%w: header for %d channels at %d Hz and %d bytes", ErrFormat, f.Channels, f.Rate, dataBytes)
	}
	h := make([]byte, HeaderBytes)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(HeaderBytes-8+dataBytes))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16)
	binary.LittleEndian.PutUint16(h[20:22], 1)
	binary.LittleEndian.PutUint16(h[22:24], f.Channels)
	binary.LittleEndian.PutUint32(h[24:28], f.Rate)
	binary.LittleEndian.PutUint32(h[28:32], f.Rate*uint32(f.blockAlign()))
	binary.LittleEndian.PutUint16(h[32:34], uint16(f.blockAlign()))
	binary.LittleEndian.PutUint16(h[34:36], 16)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataBytes))
	return h, nil
}

// Encode returns the canonical WAV of pcm in format f.
func Encode(f Format, pcm []byte) ([]byte, error) {
	h, err := Header(f, len(pcm))
	if err != nil {
		return nil, err
	}
	return append(h, pcm...), nil
}

// Decode reads a RIFF/WAVE file of at most maxBytes and returns its format and
// a copy-free view of its samples. The RIFF size must equal the length minus
// 8; the file holds exactly one fmt chunk (16 bytes, or 18 with a zero
// extension size) with format tag 1, 16 bits, one or two channels and a rate
// within MinRate..MaxRate, and exactly one data chunk after it of whole
// sample frames, at least one. Other chunks are skipped; every chunk,
// including its pad byte, must lie inside the file.
func Decode(data []byte, maxBytes int) (Format, []byte, error) {
	refuse := func(why string) (Format, []byte, error) { return Format{}, nil, fmt.Errorf("%w: %s", ErrFormat, why) }
	if len(data) > maxBytes {
		return refuse(fmt.Sprintf("larger than %d bytes", maxBytes))
	}
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return refuse("no RIFF/WAVE header")
	}
	if int64(binary.LittleEndian.Uint32(data[4:8])) != int64(len(data))-8 {
		return refuse("RIFF size is not the length minus 8")
	}
	var f Format
	var samples []byte
	seenFmt, seenData := false, false
	for pos := 12; pos < len(data); {
		if len(data)-pos < 8 {
			return refuse("truncated chunk header")
		}
		id := string(data[pos : pos+4])
		size := int64(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := int64(pos + 8)
		end := body + size + size&1
		if end > int64(len(data)) {
			return refuse("chunk " + printable(id) + " runs past the end")
		}
		switch id {
		case "fmt ":
			if seenFmt {
				return refuse("two fmt chunks")
			}
			seenFmt = true
			if size != 16 && size != 18 {
				return refuse("fmt chunk size")
			}
			b := data[body : body+size]
			tag, bits := binary.LittleEndian.Uint16(b[0:2]), binary.LittleEndian.Uint16(b[14:16])
			f = Format{Channels: binary.LittleEndian.Uint16(b[2:4]), Rate: binary.LittleEndian.Uint32(b[4:8])}
			if tag != 1 {
				return refuse(fmt.Sprintf("format tag %#x", tag))
			}
			if bits != 16 || !f.valid() {
				return refuse(fmt.Sprintf("%d bits, %d channels, %d Hz", bits, f.Channels, f.Rate))
			}
			if binary.LittleEndian.Uint32(b[8:12]) != f.Rate*uint32(f.blockAlign()) || int(binary.LittleEndian.Uint16(b[12:14])) != f.blockAlign() {
				return refuse("byte rate or block align")
			}
			if size == 18 && binary.LittleEndian.Uint16(b[16:18]) != 0 {
				return refuse("fmt extension")
			}
		case "data":
			if seenData {
				return refuse("two data chunks")
			}
			if !seenFmt {
				return refuse("data before fmt")
			}
			seenData = true
			samples = data[body : body+size]
		}
		pos = int(end)
	}
	if !seenFmt || !seenData {
		return refuse("missing fmt or data chunk")
	}
	if len(samples) == 0 || len(samples)%f.blockAlign() != 0 {
		return refuse("data is not whole sample frames")
	}
	return f, samples, nil
}

func printable(id string) string {
	for _, c := range []byte(id) {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("%q", id)
		}
	}
	return id
}
