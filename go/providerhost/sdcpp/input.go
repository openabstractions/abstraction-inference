package sdcpp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg" // the JPEG header reader for CheckImageInput
	_ "image/png"  // the PNG header reader for CheckImageInput
)

// CheckImageInput checks one input image of an edit before it is forwarded
// toward sd-server (inference CONTRACT.md INF-SD2). media is the type the
// request declares: image/png, image/jpeg or image/webp, the types inference
// sends. The width and height are read from the header alone, with
// image.DecodeConfig for PNG and JPEG and from the RIFF chunk header for
// WebP, and must meet CheckSize's upper bounds: sd-server decodes the input
// and, with size "auto", takes the request's size from it. A header that does
// not parse or
// names another format wraps ErrRequestForm; a size beyond the bound wraps
// ErrImageSize.
func CheckImageInput(media string, data []byte) error {
	width, height, err := InputSize(media, data)
	if err != nil {
		return err
	}
	// An input smaller than MinDimension is scaled up to the request's size;
	// only the upper bounds apply to it.
	return CheckSize(max(width, MinDimension), max(height, MinDimension))
}

// InputSize returns the width and height an input image's header names, read
// as CheckImageInput reads it, without decoding pixels. A header that does
// not parse as media, or names no pixels, wraps ErrRequestForm. A provider
// compares an edit mask's size with its input's (inference CONTRACT.md
// INF-I1) through it.
func InputSize(media string, data []byte) (int, int, error) {
	var width, height int
	switch media {
	case "image/png", "image/jpeg":
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || "image/"+format != media {
			return 0, 0, fmt.Errorf("%w: the input is no %s", ErrRequestForm, media)
		}
		width, height = config.Width, config.Height
	case "image/webp":
		var ok bool
		width, height, ok = webpSize(data)
		if !ok {
			return 0, 0, fmt.Errorf("%w: the input is no %s", ErrRequestForm, media)
		}
	default:
		return 0, 0, fmt.Errorf("%w: input type %q", ErrRequestForm, media)
	}
	if width < 1 || height < 1 {
		return 0, 0, fmt.Errorf("%w: the input names no pixels", ErrRequestForm)
	}
	return width, height, nil
}

// webpSize reads the canvas size of a WebP file from its first chunk: the
// lossy VP8 frame header, the lossless VP8L header or the extended VP8X
// canvas (https://developers.google.com/speed/webp/docs/riff_container).
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 20 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	payload := b[20:]
	switch string(b[12:16]) {
	case "VP8 ":
		// 3-byte frame tag, start code 9D 01 2A, then 14-bit width and height.
		if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
			return 0, 0, false
		}
		return int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff), true
	case "VP8L":
		// Signature 0x2F, then width-1 and height-1 in 14 bits each.
		if len(payload) < 5 || payload[0] != 0x2f {
			return 0, 0, false
		}
		bits := binary.LittleEndian.Uint32(payload[1:5])
		return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
	case "VP8X":
		// Flags and 3 reserved bytes, then canvas width-1 and height-1 in 24
		// bits each.
		if len(payload) < 10 {
			return 0, 0, false
		}
		width := int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
		height := int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
		return width + 1, height + 1, true
	}
	return 0, 0, false
}
