package llamacpp

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // the decoder of the JPEG images Part admits
	"image/png"
	"strings"
)

// ErrMedia is wrapped by every MediaError.
var ErrMedia = errors.New("llama.cpp: chat media refused")

// Reasons a MediaError gives.
const (
	// RefusedRemote is an http(s) URL, which llama-server fetches itself.
	RefusedRemote = "remote"
	// RefusedFile is a file: URL, which llama-server reads under --media-path.
	RefusedFile = "file"
	// RefusedType is every other refusal: a part type other than text,
	// image_url or (where admitted) input_audio, video, an image other than
	// a PNG or JPEG within the bound, audio other than WAV, MP3 or FLAC, or a
	// reference that is not base64 data.
	RefusedType = "type"
)

// MediaError names a chat medium the policy refuses.
type MediaError struct {
	Reason string // RefusedRemote, RefusedFile or RefusedType
	Detail string
}

func (e *MediaError) Error() string { return "llama.cpp media: " + e.Reason + ": " + e.Detail }

func (e *MediaError) Is(target error) bool { return target == ErrMedia }

func refuse(reason, format string, args ...any) error {
	return &MediaError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// maxImagePixels bounds the pixels of one image Part decodes and re-encodes
// (16 MP).
const maxImagePixels = 16 << 20

// Media is the chat media policy of review condition 6. llama-server fetches
// any media string starting with "http" with no host restriction, reads
// file:// paths under --media-path, and decodes video, WebP and any image its
// stb decoder rejects by spawning FFmpeg; b10825 also accepts base64 data:
// URIs for input_audio and input_video. Part admits only media the provider
// has decoded itself:
//
//   - An image_url part's URL must be a base64 data: URI declaring image/png
//     or image/jpeg whose bytes are that format, at most maxImagePixels
//     pixels, read with image.DecodeConfig before any pixel is decoded. It is
//     replaced by the provider's own 8-bit NRGBA PNG data URI, so stb parses
//     only bytes the provider produced and FFmpeg is never reached.
//   - With Audio set, an input_audio part's data and url must hold WAV, MP3
//     or FLAC bytes, recognised by their leading bytes as llama.cpp's mtmd
//     helper recognises them (miniaudio decodes those in process). data may
//     be OpenAI's raw base64 or a base64 data: URI declaring audio/*; url
//     must be a data: URI. Each is replaced by the provider's own standard
//     base64 of the same bytes: raw in data, OpenAI's form, and a data: URI
//     naming the detected type in url.
//   - text parts pass unchanged. Every other part type, video included, is
//     refused.
type Media struct {
	// Audio admits input_audio parts.
	Audio bool
}

// Part checks one content part of a chat message and rewrites its media in
// place. part is the part as encoding/json decodes it into an any.
func (m Media) Part(part any) error {
	p, ok := part.(map[string]any)
	if !ok {
		return refuse(RefusedType, "a content part is not an object")
	}
	kind, _ := p["type"].(string)
	switch kind {
	case "text":
		return nil
	case "image_url":
		media, ok := p["image_url"].(map[string]any)
		if !ok {
			return refuse(RefusedType, "image_url is not an object")
		}
		uri, err := providerImage(media["url"])
		if err != nil {
			return err
		}
		media["url"] = uri
		return nil
	case "input_audio":
		if !m.Audio {
			break
		}
		media, ok := p["input_audio"].(map[string]any)
		if !ok {
			return refuse(RefusedType, "input_audio is not an object")
		}
		_, hasData := media["data"]
		_, hasURL := media["url"]
		if !hasData && !hasURL {
			return refuse(RefusedType, "input_audio carries no data")
		}
		for _, key := range []string{"data", "url"} {
			value, ok := media[key]
			if !ok {
				continue
			}
			canonical, err := providerAudio(value, key == "data")
			if err != nil {
				return err
			}
			media[key] = canonical
		}
		return nil
	case "input_video", "video_url":
		return refuse(RefusedType, "video input needs FFmpeg, and no FFmpeg build is reviewed")
	}
	return refuse(RefusedType, "content type %q is not forwarded", kind)
}

// providerImage decodes a PNG or JPEG base64 data: URI of at most maxImagePixels
// pixels and returns it as the provider's own 8-bit NRGBA PNG data URI.
func providerImage(value any) (string, error) {
	mime, data, err := mediaBytes(value, false)
	if err != nil {
		return "", err
	}
	declared := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/jpg": "jpeg"}[mime]
	if declared == "" {
		return "", refuse(RefusedType, "only PNG and JPEG images are forwarded, not %s", mime)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" && format != "jpeg" {
		return "", refuse(RefusedType, "only PNG and JPEG images are forwarded")
	}
	if format != declared {
		return "", refuse(RefusedType, "the data is %s, not the %s it declares", format, mime)
	}
	if config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > maxImagePixels {
		return "", refuse(RefusedType, "the image has %dx%d pixels; at most %d are forwarded", config.Width, config.Height, maxImagePixels)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", refuse(RefusedType, "the image does not decode")
	}
	bounds := img.Bounds()
	nrgba := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), img, bounds.Min, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, nrgba); err != nil {
		return "", refuse(RefusedType, "the image could not be encoded: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// providerAudio checks one input_audio reference and returns the provider's own
// standard base64 of its bytes: raw where raw is set and the reference was
// raw base64, and otherwise a data: URI naming the detected type.
func providerAudio(value any, raw bool) (string, error) {
	mime, data, err := mediaBytes(value, raw)
	if err != nil {
		return "", err
	}
	if mime != "" && !strings.HasPrefix(mime, "audio/") {
		return "", refuse(RefusedType, "input_audio declares %s", mime)
	}
	detected := audioType(data)
	if detected == "" {
		return "", refuse(RefusedType, "input_audio is not WAV, MP3 or FLAC audio")
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	if mime == "" {
		return encoded, nil
	}
	return "data:" + detected + ";base64," + encoded, nil
}

// mediaBytes decodes one media reference and returns its declared type,
// empty for raw base64. http(s) and file: references are refused by name; a
// base64 data: URI is decoded, and raw base64 only where raw is set.
func mediaBytes(value any, raw bool) (string, []byte, error) {
	uri, ok := value.(string)
	if !ok {
		return "", nil, refuse(RefusedType, "the media reference is not a string")
	}
	lower := strings.ToLower(uri)
	switch {
	case strings.HasPrefix(lower, "http"):
		return "", nil, refuse(RefusedRemote, "llama-server would fetch a remote media URL")
	case strings.HasPrefix(lower, "file:"):
		return "", nil, refuse(RefusedFile, "llama-server would read a local media file")
	}
	payload, mime := uri, ""
	if strings.HasPrefix(lower, "data:") {
		header, rest, ok := strings.Cut(uri, ",")
		if !ok || strings.Contains(rest, ",") || !strings.HasSuffix(strings.ToLower(header), ";base64") {
			return "", nil, refuse(RefusedType, "media must be one base64 data: URI")
		}
		mime, _, _ = strings.Cut(strings.ToLower(header[len("data:"):]), ";")
		switch {
		case strings.HasPrefix(mime, "video/"):
			return "", nil, refuse(RefusedType, "video input needs FFmpeg, and no FFmpeg build is reviewed")
		case !strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "audio/"):
			return "", nil, refuse(RefusedType, "media type %s is not forwarded", mime)
		}
		payload = rest
	} else if !raw {
		return "", nil, refuse(RefusedType, "media must be a base64 data: URI")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(payload)
	}
	if err != nil || len(data) == 0 {
		return "", nil, refuse(RefusedType, "the media data is not base64")
	}
	return mime, data, nil
}

// audioType is llama.cpp b10825's audio_helpers::is_audio_file
// (tools/mtmd/mtmd-helper.cpp), naming the type it recognised: bytes it
// treats as audio are decoded by miniaudio or refused, never handed to
// FFmpeg.
func audioType(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	switch {
	case string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return "audio/wav"
	case string(b[:4]) == "fLaC":
		return "audio/flac"
	case string(b[:3]) == "ID3" || b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	}
	return ""
}
