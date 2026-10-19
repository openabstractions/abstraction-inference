package llamacpp

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func encoded(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "jpeg":
		err = jpeg.Encode(&buf, img, nil)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uri(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// wav is the shortest header llama.cpp's mtmd helper recognises as audio.
var wav = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 28)...)

func imagePart(url any) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func audioPart(key string, value any) map[string]any {
	return map[string]any{"type": "input_audio", "input_audio": map[string]any{key: value, "format": "wav"}}
}

func reason(err error) string {
	var m *MediaError
	if !errors.As(err, &m) || !errors.Is(err, ErrMedia) {
		return ""
	}
	return m.Reason
}

// Review condition 6: no reference llama-server would fetch, read or hand to
// FFmpeg passes, and each refusal names its reason.
func TestMediaRefusals(t *testing.T) {
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 8)...)
	bmp := append([]byte("BM6\x00\x00\x00"), make([]byte, 48)...)
	big := image.NewGray(image.Rect(0, 0, 4097, 4097))
	var bigPNG bytes.Buffer
	if err := png.Encode(&bigPNG, big); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		part   any
		reason string
	}{
		"http image":        {imagePart("http://127.0.0.1:8080/x.png"), RefusedRemote},
		"HTTPS image":       {imagePart("HTTPS://example.invalid/x.png"), RefusedRemote},
		"http-prefixed":     {imagePart("httpfoo"), RefusedRemote},
		"file image":        {imagePart("file:///C:/Windows/win.ini"), RefusedFile},
		"FILE image":        {imagePart("FILE:C:/x.png"), RefusedFile},
		"bare path":         {imagePart(`C:\x\a.png`), RefusedType},
		"raw base64 image":  {imagePart(base64.StdEncoding.EncodeToString(encoded(t, "png", 2, 2))), RefusedType},
		"not a string":      {imagePart(42), RefusedType},
		"image not object":  {map[string]any{"type": "image_url", "image_url": "https://example.invalid/x.png"}, RefusedType},
		"WebP":              {imagePart(uri("image/webp", webp)), RefusedType},
		"WebP as PNG":       {imagePart(uri("image/png", webp)), RefusedType},
		"GIF":               {imagePart(uri("image/gif", encoded(t, "gif", 2, 2))), RefusedType},
		"GIF as PNG":        {imagePart(uri("image/png", encoded(t, "gif", 2, 2))), RefusedType},
		"BMP":               {imagePart(uri("image/bmp", bmp)), RefusedType},
		"SVG":               {imagePart(uri("image/svg+xml", []byte("<svg/>"))), RefusedType},
		"JPEG as PNG":       {imagePart(uri("image/png", encoded(t, "jpeg", 2, 2))), RefusedType},
		"PNG as JPEG":       {imagePart(uri("image/jpeg", encoded(t, "png", 2, 2))), RefusedType},
		"PNG as audio":      {imagePart(uri("audio/wav", encoded(t, "png", 2, 2))), RefusedType},
		"truncated PNG":     {imagePart(uri("image/png", []byte("\x89PNG\r\n\x1a\nnot an image"))), RefusedType},
		"empty data":        {imagePart("data:image/png;base64,"), RefusedType},
		"not base64":        {imagePart("data:image/png;base64,!!!!"), RefusedType},
		"two commas":        {imagePart("data:image/png;base64,AAAA,AAAA"), RefusedType},
		"no base64":         {imagePart("data:image/png,AAAA"), RefusedType},
		"over 16 MP":        {imagePart(uri("image/png", bigPNG.Bytes())), RefusedType},
		"video data":        {imagePart(uri("video/mp4", []byte("....ftypisom"))), RefusedType},
		"video part":        {map[string]any{"type": "input_video", "input_video": map[string]any{"data": uri("video/mp4", []byte("x"))}}, RefusedType},
		"video_url part":    {map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.invalid/v.mp4"}}, RefusedType},
		"file part":         {map[string]any{"type": "file", "file": map[string]any{"file_data": "x"}}, RefusedType},
		"untyped part":      {map[string]any{"image_url": map[string]any{"url": "http://10.0.0.1/a.png"}}, RefusedType},
		"part not object":   {"text", RefusedType},
		"https audio":       {audioPart("url", "https://example.invalid/a.wav"), RefusedRemote},
		"raw audio url":     {audioPart("url", base64.StdEncoding.EncodeToString(wav)), RefusedType},
		"PNG as audio data": {audioPart("data", base64.StdEncoding.EncodeToString(encoded(t, "png", 2, 2))), RefusedType},
		"text as audio":     {audioPart("data", uri("audio/wav", []byte("not audio at all"))), RefusedType},
		"audio as image":    {audioPart("data", uri("image/png", wav)), RefusedType},
		"audio not base64":  {audioPart("data", "!!not base64!!"), RefusedType},
		"audio empty":       {map[string]any{"type": "input_audio", "input_audio": map[string]any{"format": "wav"}}, RefusedType},
		"audio not object":  {map[string]any{"type": "input_audio", "input_audio": "x"}, RefusedType},
	} {
		t.Run(name, func(t *testing.T) {
			err := Media{Audio: true}.Part(c.part)
			if got := reason(err); got != c.reason {
				t.Fatalf("%v: reason %q, want %q", err, got, c.reason)
			}
		})
	}
}

// Every image is decoded and re-encoded as an 8-bit NRGBA PNG data URI.
func TestImagesAreReencodedAsProviderPNG(t *testing.T) {
	for name, given := range map[string]string{
		"PNG":       uri("image/png", encoded(t, "png", 3, 2)),
		"JPEG":      uri("image/jpeg", encoded(t, "jpeg", 33, 17)),
		"JPG alias": uri("image/jpg", encoded(t, "jpeg", 5, 5)),
		"upper":     strings.Replace(uri("image/png", encoded(t, "png", 3, 2)), "data:image/png;base64", "DATA:IMAGE/PNG;BASE64", 1),
	} {
		part := imagePart(given)
		if err := (Media{}).Part(part); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
		out := part["image_url"].(map[string]any)["url"].(string)
		payload, ok := strings.CutPrefix(out, "data:image/png;base64,")
		if !ok {
			t.Fatalf("%s forwarded as %.30s", name, out)
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatal(err)
		}
		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil || format != "png" || img.Bounds().Min != (image.Point{}) {
			t.Fatalf("%s: %s %v", name, format, err)
		}
		// IHDR's bit depth byte.
		if data[24] != 8 {
			t.Fatalf("%s: bit depth %d", name, data[24])
		}
		if name == "JPEG" && out == given {
			t.Fatal("the caller's JPEG was forwarded")
		}
	}
}

// Audio is admitted only where the provider admits it, and forwarded as the
// provider's own standard base64: raw in data, a data: URI in url.
func TestAudioIsForwardedAsCanonicalBase64(t *testing.T) {
	part := audioPart("data", base64.StdEncoding.EncodeToString(wav))
	if err := (Media{}).Part(part); reason(err) != RefusedType {
		t.Fatalf("audio admitted without Audio: %v", err)
	}
	raw := base64.RawStdEncoding.EncodeToString(wav)
	for name, c := range map[string]struct {
		key, given, want string
	}{
		"raw":             {"data", raw, base64.StdEncoding.EncodeToString(wav)},
		"data URI":        {"data", uri("audio/x-wav", wav), "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(wav)},
		"url data URI":    {"url", uri("audio/wav", wav), "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(wav)},
		"flac":            {"data", base64.StdEncoding.EncodeToString(append([]byte("fLaC"), make([]byte, 12)...)), base64.StdEncoding.EncodeToString(append([]byte("fLaC"), make([]byte, 12)...))},
		"mp3 as data URI": {"url", uri("audio/mpeg", append([]byte("ID3"), make([]byte, 13)...)), "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString(append([]byte("ID3"), make([]byte, 13)...))},
	} {
		part := audioPart(c.key, c.given)
		if err := (Media{Audio: true}).Part(part); err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
		if got := part["input_audio"].(map[string]any)[c.key]; got != c.want {
			t.Fatalf("%s forwarded as %.40v, want %.40v", name, got, c.want)
		}
	}
	// Both references are read and both are checked.
	both := map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(wav), "url": "https://example.invalid/a.wav"}}
	if err := (Media{Audio: true}).Part(both); reason(err) != RefusedRemote {
		t.Fatalf("a remote url beside data passed: %v", err)
	}
}

func TestTextPartsPassUnchanged(t *testing.T) {
	part := map[string]any{"type": "text", "text": "see http://example.invalid/a.png"}
	if err := (Media{}).Part(part); err != nil || part["text"] != "see http://example.invalid/a.png" {
		t.Fatalf("%v %v", err, part)
	}
}
