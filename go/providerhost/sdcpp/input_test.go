package sdcpp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

// pngHeader is a PNG signature and IHDR chunk naming width × height; the
// header alone is what image.DecodeConfig reads.
func pngHeader(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8], ihdr[9] = 8, 6 // 8-bit RGBA
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func webpFile(chunk string, payload []byte) []byte {
	body := append([]byte(chunk), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(body[4:8], uint32(len(payload)))
	body = append(body, payload...)
	out := append([]byte("RIFF"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:8], uint32(4+len(body)))
	return append(append(out, "WEBP"...), body...)
}

// Addendum finding V1: an edit's input image is bounded by its header before
// sd-server decodes it, as the request size is.
func TestImageInputBound(t *testing.T) {
	var small bytes.Buffer
	if err := png.Encode(&small, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 640, 480)), nil); err != nil {
		t.Fatal(err)
	}
	lossy := make([]byte, 10)
	lossy[3], lossy[4], lossy[5] = 0x9d, 0x01, 0x2a
	binary.LittleEndian.PutUint16(lossy[6:8], 1024)
	binary.LittleEndian.PutUint16(lossy[8:10], 768)
	lossless := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(lossless[1:5], uint32(511)|uint32(511)<<14)
	extended := []byte{0, 0, 0, 0, 0xff, 0x07, 0, 0xff, 0x07, 0} // 2048 × 2048
	for name, input := range map[string]struct {
		media string
		data  []byte
	}{
		"png": {"image/png", small.Bytes()}, "png header 2048": {"image/png", pngHeader(2048, 2048)}, "png 1x1": {"image/png", pngHeader(1, 1)},
		"jpeg": {"image/jpeg", photo.Bytes()}, "webp lossy": {"image/webp", webpFile("VP8 ", lossy)},
		"webp lossless": {"image/webp", webpFile("VP8L", lossless)}, "webp extended": {"image/webp", webpFile("VP8X", extended)},
	} {
		if err := CheckImageInput(input.media, input.data); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
	}
	hugeExtended := []byte{0, 0, 0, 0, 0xff, 0x3f, 0, 0xff, 0x3f, 0} // 16384 × 16384
	for name, input := range map[string]struct {
		media string
		data  []byte
	}{
		"png 16384": {"image/png", pngHeader(16384, 16384)}, "png 4096 square": {"image/png", pngHeader(4096, 4096)},
		"png 65536 wide": {"image/png", pngHeader(65536, 64)}, "webp 16384": {"image/webp", webpFile("VP8X", hugeExtended)},
	} {
		err := CheckImageInput(input.media, input.data)
		if !errors.Is(err, ErrImageSize) || Reason(err) != ReasonImageSize {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, input := range map[string]struct {
		media string
		data  []byte
	}{
		"png as jpeg": {"image/jpeg", small.Bytes()}, "gif": {"image/gif", []byte("GIF89a")}, "truncated": {"image/png", small.Bytes()[:12]},
		"webp short": {"image/webp", []byte("RIFF\x00\x00\x00\x00WEBP")}, "webp bad start code": {"image/webp", webpFile("VP8 ", make([]byte, 10))},
		"webp other chunk": {"image/webp", webpFile("ALPH", make([]byte, 10))}, "empty": {"image/png", nil},
		"webp no pixels": {"image/webp", webpFile("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0})},
	} {
		err := CheckImageInput(input.media, input.data)
		if !errors.Is(err, ErrRequestForm) || Reason(err) != ReasonRequestForm {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// InputSize names the header's size for each input type and refuses what
// CheckImageInput refuses as a form, before any pixel is decoded.
func TestInputSizeReadsTheHeader(t *testing.T) {
	if w, h, err := InputSize("image/png", pngHeader(640, 360)); err != nil || w != 640 || h != 360 {
		t.Fatalf("png %dx%d %v", w, h, err)
	}
	if w, h, err := InputSize("image/webp", webpFile("VP8X", []byte{0, 0, 0, 0, 0x7f, 0x02, 0, 0x67, 0x01, 0})); err != nil || w != 640 || h != 360 {
		t.Fatalf("webp %dx%d %v", w, h, err)
	}
	for name, input := range map[string]struct {
		media string
		data  []byte
	}{"no pixels": {"image/png", pngHeader(0, 5)}, "other type": {"image/gif", []byte("GIF89a")}, "jpeg as png": {"image/png", []byte("\xff\xd8\xff\xe0")}} {
		if _, _, err := InputSize(input.media, input.data); !errors.Is(err, ErrRequestForm) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
