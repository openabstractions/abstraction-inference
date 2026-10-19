// Package mediationcontent transfers verified image, audio and video content
// between native provider endpoints and an inference service through public
// storage IPC.
package mediationcontent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	api "github.com/openabstractions/abstraction-storage/go/abstraction/storage/content"
	storageclient "github.com/openabstractions/abstraction-storage/go/client"
)

const chunkBytes = 65536

func validDigest(digest string) bool {
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	for _, c := range digest[7:] {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

// audioMedia reports the audio types transcription input and speech output
// carry.
func audioMedia(media string) bool {
	switch media {
	case "audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac", "audio/aac", "audio/pcm":
		return true
	}
	return false
}

// MediaMatches reports whether data is of media type media by its content:
// an image or video by its detected type, an audio container by its leading
// bytes, and audio/pcm (16-bit samples) by a non-zero, even length. It is the
// one content check of the media types Download and Upload carry.
func MediaMatches(data []byte, media string) bool {
	if ImageMedia(media) || VideoMedia(media) {
		return http.DetectContentType(data) == media
	}
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

// ImageMedia reports whether media is an image type inference accepts as an
// input or a generated image: PNG, JPEG or WebP.
func ImageMedia(media string) bool {
	return media == "image/png" || media == "image/jpeg" || media == "image/webp"
}

// VideoMedia reports whether media is a container inference accepts as a
// generated video: MP4, WebM or AVI, each named as net/http's
// DetectContentType names it (AVI is video/avi).
func VideoMedia(media string) bool {
	return media == "video/mp4" || media == "video/webm" || media == "video/avi"
}

// SniffedMedia returns the name DetectContentType gives the type an engine
// declared: video/x-msvideo, AVI's registered name, becomes video/avi. Every
// other value is returned unchanged.
func SniffedMedia(media string) string {
	if media == "video/x-msvideo" {
		return "video/avi"
	}
	return media
}

// ErrRefusedLocally wraps an Upload refusal made before any call reaches the
// provider: bounds, media type or a digest that does not match the bytes.
// Every other Upload error came after a call and leaves the provider's state
// unknown.
var ErrRefusedLocally = errors.New("content: refused before transfer")

// validMedia is what Download reads: an image, audio or a video.
func validMedia(media string) bool {
	return ImageMedia(media) || audioMedia(media) || VideoMedia(media)
}

// uploadMedia is what Upload stages: caller input images and audio, and a
// provider's own speech output.
func uploadMedia(media string) bool {
	return ImageMedia(media) || audioMedia(media)
}

func verify(data []byte, digest, media string) error {
	sum := sha256.Sum256(data)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return errors.New("content: digest mismatch")
	}
	if !MediaMatches(data, media) {
		return errors.New("content: media type mismatch")
	}
	return nil
}

// Download reads one declared image, audio object or video from the
// provider's content-reader@1.
// maxBytes must be the smaller of the per-image and remaining total budget.
// The returned bytes are safe for a separate caller-authorized content commit.
func Download(ctx context.Context, transport listen.FrameClient, digest, media string, size, maxBytes int64) ([]byte, error) {
	if ctx == nil || !validDigest(digest) || !validMedia(media) || size < 1 || maxBytes < 1 || size > maxBytes || size > int64(int(^uint(0)>>1)) {
		return nil, errors.New("content: invalid download bounds")
	}
	reader := storageclient.NewWithTransport(transport)
	opened, err := reader.Open(ctx, digest)
	if err != nil {
		return nil, err
	}
	if opened.Outcome != api.OpenOutcomeOpened || opened.Resource == nil {
		return nil, fmt.Errorf("content: open %s", opened.Outcome)
	}
	resource := *opened.Resource
	// Release belongs to the same authenticated program even after caller
	// cancellation. The storage contract permits Close after read revocation.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = reader.Close(releaseCtx, resource)
	}()
	if resource.Size != size || resource.Size > maxBytes {
		return nil, errors.New("content: provider size mismatch")
	}
	data := make([]byte, 0, int(size))
	for int64(len(data)) < size {
		offset := int64(len(data))
		read, err := reader.Read(ctx, resource, offset, min(chunkBytes, size-offset))
		if err != nil {
			return nil, err
		}
		if read.Outcome != api.ReadOutcomeData || read.Chunk == nil || len(read.Chunk.Data) == 0 {
			return nil, fmt.Errorf("content: read %s", read.Outcome)
		}
		data = append(data, read.Chunk.Data...)
	}
	if err := verify(data, digest, media); err != nil {
		return nil, err
	}
	return data, nil
}

// Upload stages preverified caller input at the provider's content-writer@1.
// requestID is stable for this logical transfer. Begin, Append and Commit are
// each sent once per step; an uncertain result is returned for explicit caller
// reconciliation and never causes an automatic retry or image execution.
func Upload(ctx context.Context, transport listen.FrameClient, requestID, digest, media string, data []byte, maxBytes int64) error {
	if ctx == nil || !validDigest(digest) || !uploadMedia(media) || len(data) < 1 || maxBytes < 1 || int64(len(data)) > maxBytes {
		return fmt.Errorf("%w: invalid upload bounds", ErrRefusedLocally)
	}
	if err := verify(data, digest, media); err != nil {
		return fmt.Errorf("%w: %v", ErrRefusedLocally, err)
	}
	writer := storageclient.NewWriterWithTransport(transport)
	begin, err := writer.Begin(ctx, requestID, digest, int64(len(data)))
	if err != nil {
		return err
	}
	switch begin.Outcome {
	case api.BeginOutcomeCommitted, api.BeginOutcomePresent:
		// A resumed committed identity and a naming claim both need live
		// content at this provider before image execution can start.
		present, err := Download(ctx, transport, digest, media, int64(len(data)), maxBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(present, data) {
			return errors.New("content: provider present bytes differ")
		}
		return nil
	case api.BeginOutcomeStarted:
	default:
		return fmt.Errorf("content: begin %s", begin.Outcome)
	}
	if begin.Upload == nil {
		return errors.New("content: missing upload")
	}
	upload := *begin.Upload
	for offset := upload.Received; offset < int64(len(data)); {
		end := min(offset+chunkBytes, int64(len(data)))
		appended, err := writer.Append(ctx, upload, offset, data[offset:end])
		if err != nil {
			return err
		}
		if appended.Outcome != api.AppendOutcomeAccepted || appended.Received != end {
			return fmt.Errorf("content: append %s", appended.Outcome)
		}
		offset = end
	}
	committed, err := writer.Commit(ctx, upload)
	if err != nil {
		return err
	}
	if committed.Outcome != api.CommitOutcomeCommitted {
		return fmt.Errorf("content: commit %s", committed.Outcome)
	}
	return nil
}
