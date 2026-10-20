// Package inputprofile names the request input profile content falls under
// (inference CONTRACT.md INF-V4) by its leading bytes. It is the one
// classification the inference provider, the native relays and an uploading
// client share, and it imports only the standard library.
package inputprofile

import (
	"bytes"
	"net/http"
)

// Request input profiles. Content an application uploads for a request is
// written and read back under one of these bounded profiles.
const (
	// Image is an edit input, an edit mask, a vision image or a video's
	// first frame.
	Image = "image"
	// Audio is audio to transcribe.
	Audio = "audio"
)

// HeadBytes is how many leading bytes Of reads.
const HeadBytes = 512

// Resource is the rights resource of an input profile, input:<profile>,
// under both abstraction.storage/content.write and content.read.
func Resource(profile string) string { return "input:" + profile }

// transcriptionAudio lists the audio types transcription accepts (INF-M5).
var transcriptionAudio = []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac"}

// Of names the input profile the leading bytes of a content object fall
// under, or "" when they fit none. head needs at most HeadBytes bytes. An
// image is a PNG, JPEG, GIF or WebP, the types image input and vision accept
// (INF-V1); audio is any type transcription accepts (INF-M5).
func Of(head []byte) string {
	switch http.DetectContentType(head) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return Image
	}
	for _, media := range transcriptionAudio {
		if AudioMatches(head, media) {
			return Audio
		}
	}
	return ""
}

// AudioMatches reports whether data is of audio type media by its content:
// an audio container by its leading bytes, and audio/pcm (16-bit samples) by
// a non-zero, even length. Any other media type matches nothing.
func AudioMatches(data []byte, media string) bool {
	switch media {
	case "audio/wav":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	case "audio/mpeg":
		return len(data) >= 3 && (string(data[:3]) == "ID3" || data[0] == 0xff && data[1]&0xe0 == 0xe0)
	case "audio/mp4":
		return len(data) >= 12 && string(data[4:8]) == "ftyp"
	case "audio/webm":
		return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3})
	case "audio/ogg":
		return len(data) >= 4 && string(data[:4]) == "OggS"
	case "audio/flac":
		return len(data) >= 4 && string(data[:4]) == "fLaC"
	case "audio/aac":
		return len(data) >= 2 && data[0] == 0xff && data[1]&0xf6 == 0xf0
	case "audio/pcm":
		return len(data) > 0 && len(data)%2 == 0
	}
	return false
}
