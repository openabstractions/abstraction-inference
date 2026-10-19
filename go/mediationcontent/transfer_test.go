package mediationcontent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	casapi "github.com/openabstractions/abstraction-cas/go/api"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageservice "github.com/openabstractions/abstraction-storage/go/service"
)

func pngFixture() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 70000)...)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum)
}

func relayFixture(t *testing.T, readPolicy storageservice.Policy) (listen.FrameClient, *storage.ContentStore) {
	t.Helper()
	store, err := storage.NewContentStore("fixture", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if readPolicy == nil {
		readPolicy = func(context.Context, *identity.Peer, string) error { return nil }
	}
	endpoint := filepath.Join(t.TempDir(), "content.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\oa-image-content-%d`, time.Now().UnixNano())
	}
	// A macOS socket endpoint cannot carry the Program proof the content
	// service requires; the installed runtime serves storage-content-v1 over
	// XPC there.
	if err := listen.CanEver(endpoint, listen.Program); err != nil {
		t.Skipf("this platform's socket endpoint cannot carry Program proof: %v", err)
	}
	host, err := storageservice.Listen(endpoint, store, readPolicy)
	if err != nil {
		t.Fatal(err)
	}
	writePolicy := func(context.Context, *identity.Peer, string) error { return nil }
	if err := host.EnableWriter(writePolicy, 1<<20, casapi.BoundedFileStore{MaxBytes: storageservice.MaxRecordFileBytes}, filepath.Join(t.TempDir(), "records.json")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		host.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("storage host did not stop")
		}
	})
	return listen.FrameClient{Endpoint: endpoint}.WithDefaults(5*time.Second, 1<<20), store
}

func TestUploadDownloadVerifiedThroughPublicIPC(t *testing.T) {
	transport, _ := relayFixture(t, nil)
	data := pngFixture()
	digest := digestOf(data)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Upload(ctx, transport, "stable-request-0001", digest, "image/png", data, 100000); err != nil {
		t.Fatal(err)
	}
	got, err := Download(ctx, transport, digest, "image/png", int64(len(data)), 100000)
	if err != nil || string(got) != string(data) {
		t.Fatalf("download %d bytes: %v", len(got), err)
	}
	if err := Upload(ctx, transport, "stable-request-0001", digest, "image/png", data, 100000); err != nil {
		t.Fatalf("committed identity: %v", err)
	}
	if err := Upload(ctx, transport, "another-request-001", digest, "image/png", data, 100000); err != nil {
		t.Fatalf("verified present object: %v", err)
	}
}

func TestTransferRejectsBoundsDigestAndMedia(t *testing.T) {
	transport, _ := relayFixture(t, nil)
	data := pngFixture()
	digest := digestOf(data)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Upload(ctx, transport, "stable-request-0002", digest, "image/png", data, int64(len(data))-1); err == nil {
		t.Fatal("oversized input accepted")
	}
	if err := Upload(ctx, transport, "stable-request-0002", digest, "image/jpeg", data, 100000); err == nil {
		t.Fatal("wrong input media accepted")
	}
	if err := Upload(ctx, transport, "stable-request-0002", digestOf([]byte("different")), "image/png", data, 100000); err == nil {
		t.Fatal("wrong input digest accepted")
	}
	if err := Upload(ctx, transport, "stable-request-0002", digest, "image/png", data, 100000); err != nil {
		t.Fatal(err)
	}
	if _, err := Download(ctx, transport, digest, "image/png", int64(len(data)), int64(len(data))-1); err == nil {
		t.Fatal("oversized output accepted")
	}
	if _, err := Download(ctx, transport, digest, "image/png", int64(len(data))-1, 100000); err == nil {
		t.Fatal("wrong size accepted")
	}
	if _, err := Download(ctx, transport, digest, "image/jpeg", int64(len(data)), 100000); err == nil {
		t.Fatal("wrong output media accepted")
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Download(cancelled, transport, digest, "image/png", int64(len(data)), 100000); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

// TestVideoDeliveryDownloadsAndNeverUploads: a generated video is read back
// from a provider store under its sniffed name; caller inputs stay images, so
// Upload refuses a video type. The media rule agrees with net/http's sniffer.
func TestVideoDeliveryDownloadsAndNeverUploads(t *testing.T) {
	webm := append([]byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01"), make([]byte, 70000)...)
	avi := append([]byte("RIFF\x24\x00\x00\x00AVI LIST"), make([]byte, 64)...)
	mp4 := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	for media, data := range map[string][]byte{"video/webm": webm, "video/avi": avi, "video/mp4": mp4} {
		if got := http.DetectContentType(data); got != media || !VideoMedia(media) || ImageMedia(media) {
			t.Fatalf("%s sniffs as %s", media, got)
		}
	}
	if SniffedMedia("video/x-msvideo") != "video/avi" || SniffedMedia("video/webm") != "video/webm" || VideoMedia("video/x-msvideo") {
		t.Fatal("AVI's registered name is not mapped to the sniffed name")
	}
	transport, store := relayFixture(t, nil)
	digest := digestOf(webm)
	ref, err := store.Place(digest, int64(len(webm)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(ref), webm, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(ref); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := Download(ctx, transport, digest, "video/webm", int64(len(webm)), 1<<20)
	if err != nil || string(got) != string(webm) {
		t.Fatalf("download %d bytes: %v", len(got), err)
	}
	if _, err := Download(ctx, transport, digest, "video/mp4", int64(len(webm)), 1<<20); err == nil {
		t.Fatal("WebM bytes accepted as MP4")
	}
	if err := Upload(ctx, transport, "stable-request-0009", digest, "video/webm", webm, 1<<20); !errors.Is(err, ErrRefusedLocally) {
		t.Fatalf("video as a caller input: %v, want a local refusal", err)
	}
}

// TestUploadSeparatesLocalRefusalFromTransport: a refusal made before any
// call is ErrRefusedLocally; a call that fails leaves the provider's state
// unknown and is a different error.
func TestUploadSeparatesLocalRefusalFromTransport(t *testing.T) {
	data := pngFixture()
	digest := digestOf(data)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unreachable := listen.FrameClient{Endpoint: filepath.Join(t.TempDir(), "absent.sock")}.WithDefaults(time.Second, 1<<20)
	if runtime.GOOS == "windows" {
		unreachable = listen.FrameClient{Endpoint: fmt.Sprintf(`\\.\pipe\oa-absent-%d`, time.Now().UnixNano())}.WithDefaults(time.Second, 1<<20)
	}
	if err := Upload(ctx, unreachable, "stable-request-0010", digestOf([]byte("other")), "image/png", data, 100000); !errors.Is(err, ErrRefusedLocally) {
		t.Fatalf("digest mismatch: %v, want a local refusal", err)
	}
	if err := Upload(ctx, unreachable, "stable-request-0010", digest, "image/png", data, 10); !errors.Is(err, ErrRefusedLocally) {
		t.Fatalf("oversize: %v, want a local refusal", err)
	}
	if err := Upload(ctx, unreachable, "stable-request-0010", digest, "image/png", data, 100000); err == nil || errors.Is(err, ErrRefusedLocally) {
		t.Fatalf("unreachable provider: %v, want a transport error", err)
	}
}

func TestPresentObjectNeedsReadAuthorization(t *testing.T) {
	var deny atomic.Bool
	transport, _ := relayFixture(t, func(context.Context, *identity.Peer, string) error {
		if deny.Load() {
			return errors.New("read denied")
		}
		return nil
	})
	data := pngFixture()
	digest := digestOf(data)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Upload(ctx, transport, "stable-request-0003", digest, "image/png", data, 100000); err != nil {
		t.Fatal(err)
	}
	deny.Store(true)
	if err := Upload(ctx, transport, "another-request-003", digest, "image/png", data, 100000); err == nil {
		t.Fatal("unverified present object accepted")
	}
}

// Speech and transcription relay audio the same way: each audio media type is
// checked by its leading bytes, PCM by its even length.
func TestAudioTransferChecksMediaByContent(t *testing.T) {
	transport, _ := relayFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 28)...)
	pcm := []byte{1, 0, 2, 0}
	for i, c := range []struct {
		media string
		data  []byte
	}{{"audio/wav", wav}, {"audio/pcm", pcm}, {"audio/mpeg", []byte("ID3\x04abc")}, {"audio/ogg", []byte("OggSabc")}, {"audio/flac", []byte("fLaCabc")}} {
		digest := digestOf(c.data)
		if err := Upload(ctx, transport, fmt.Sprintf("audio-request-%04d", i), digest, c.media, c.data, 1000); err != nil {
			t.Fatalf("%s: %v", c.media, err)
		}
		got, err := Download(ctx, transport, digest, c.media, int64(len(c.data)), 1000)
		if err != nil || string(got) != string(c.data) {
			t.Fatalf("%s: %v", c.media, err)
		}
	}
	if !MediaMatches(wav, "audio/wav") || MediaMatches(pcm, "audio/wav") || MediaMatches([]byte{1, 0, 2}, "audio/pcm") || MediaMatches(wav, "audio/x-unknown") {
		t.Fatal("media matching")
	}
	if err := Upload(ctx, transport, "audio-request-wrong", digestOf(pcm), "audio/wav", pcm, 1000); err == nil {
		t.Fatal("PCM accepted as WAV")
	}
}
