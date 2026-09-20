package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type fakeImage struct {
	request  wire.ImageRequest
	delivery wire.Delivery
}

func (*fakeImage) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{}
}
func (*fakeImage) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*fakeImage) Cancel(inference.Subject, string) wire.Cancellation { return wire.Cancellation{} }
func (f *fakeImage) StartImage(_ context.Context, _ inference.Subject, request wire.ImageRequest) wire.Admission {
	f.request = request
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "image-op"}
}
func (f *fakeImage) ObserveImage(_ context.Context, _ inference.Subject, _ string, cursor, _, _, _ int64) wire.DeltaPage {
	if cursor == 0 {
		return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 0, Kind: wire.DeltaKindImageResult, ImageResult: &wire.ImageResult{Index: 0, Delivery: f.delivery}}}, Next: 1}
	}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 1, Kind: wire.DeltaKindEnd, ImageEnd: &wire.ImageReply{Outcome: wire.ReplyOutcomeCompleted, Deliveries: []wire.Delivery{f.delivery}, Images: 1}}}, Next: 2, AtEnd: true}
}
func (*fakeImage) CancelImage(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

func TestGatewayImageGenerationRoundTripsBase64(t *testing.T) {
	image := []byte("\x89PNG\r\n\x1a\ngateway-image")
	sum := sha256.Sum256(image)
	digest := fmt.Sprintf("sha256:%x", sum)
	provider := &fakeImage{delivery: wire.Delivery{Digest: digest, MediaType: "image/png", Size: int64(len(image)), Location: "local"}}
	subject := inference.Subject{Account: "account", Program: "/apps/images"}
	w := &Window{cfg: Config{Chat: provider, ReadContent: func(_ context.Context, got inference.Subject, gotDigest string, limit int64) ([]byte, inference.ContentOutcome) {
		if got != subject || gotDigest != digest || limit != int64(len(image)) {
			t.Fatalf("read subject=%+v digest=%q limit=%d", got, gotDigest, limit)
		}
		return image, inference.ContentResolved
	}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"lighthouse","n":1,"size":"1024x1024","response_format":"b64_json"}`))
	rw := httptest.NewRecorder()
	w.openAIImageGeneration(rw, r, fixedBound{subject}, Grant{Credential: "openai"})
	if rw.Code != http.StatusOK || !strings.Contains(rw.Body.String(), base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("code=%d body=%s", rw.Code, rw.Body.String())
	}
	if provider.request.Mode != wire.ImageModeGenerate || provider.request.Count != 1 || provider.request.Credential != "openai" {
		t.Fatalf("request %+v", provider.request)
	}
}

func TestGatewayImageDoesNotReadRemoteDeliveryLocally(t *testing.T) {
	delivery := wire.Delivery{Digest: "sha256:" + strings.Repeat("1", 64), MediaType: "image/png", Size: 10, Location: "remote.example"}
	provider := &fakeImage{delivery: delivery}
	reads := 0
	w := &Window{cfg: Config{Chat: provider, ReadContent: func(context.Context, inference.Subject, string, int64) ([]byte, inference.ContentOutcome) {
		reads++
		return nil, inference.ContentUnknown
	}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"lighthouse"}`))
	rw := httptest.NewRecorder()
	w.openAIImageGeneration(rw, r, fixedBound{inference.Subject{Account: "account", Program: "/apps/images"}}, Grant{Credential: "openai"})
	if rw.Code != http.StatusServiceUnavailable || reads != 0 {
		t.Fatalf("code=%d reads=%d body=%s", rw.Code, reads, rw.Body.String())
	}
}

func TestGatewayImageEditStoresInputsForOriginalSubject(t *testing.T) {
	output := []byte("\x89PNG\r\n\x1a\nresult")
	sum := sha256.Sum256(output)
	digest := fmt.Sprintf("sha256:%x", sum)
	provider := &fakeImage{delivery: wire.Delivery{Digest: digest, MediaType: "image/png", Size: int64(len(output)), Location: "local"}}
	subject := inference.Subject{Account: "account", Program: "/apps/editor"}
	stored := map[string][]byte{}
	w := &Window{cfg: Config{
		Chat: provider,
		StoreContent: func(_ context.Context, got inference.Subject, gotDigest string, data []byte) inference.ContentOutcome {
			if got != subject {
				t.Fatalf("write subject=%+v", got)
			}
			stored[gotDigest] = append([]byte(nil), data...)
			return inference.ContentResolved
		},
		ReadContent: func(_ context.Context, got inference.Subject, gotDigest string, _ int64) ([]byte, inference.ContentOutcome) {
			if got != subject || gotDigest != digest {
				t.Fatalf("read subject=%+v digest=%q", got, gotDigest)
			}
			return output, inference.ContentResolved
		},
	}}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, value := range map[string]string{"model": "gpt-image-1", "prompt": "remove the sign", "n": "1", "size": "1024x1024"} {
		_ = mw.WriteField(name, value)
	}
	imagePart, _ := mw.CreateFormFile("image", "input.png")
	_, _ = imagePart.Write([]byte("\x89PNG\r\n\x1a\ninput"))
	maskPart, _ := mw.CreateFormFile("mask", "mask.png")
	_, _ = maskPart.Write([]byte("\x89PNG\r\n\x1a\nmask"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rw := httptest.NewRecorder()
	w.openAIImageEdit(rw, r, fixedBound{subject}, Grant{Credential: "openai"})
	if rw.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rw.Code, rw.Body.String())
	}
	if provider.request.Mode != wire.ImageModeEdit || provider.request.ImageDigest == "" || provider.request.MaskDigest == "" {
		t.Fatalf("request %+v", provider.request)
	}
	if len(stored) != 2 || !bytes.Equal(stored[provider.request.ImageDigest], []byte("\x89PNG\r\n\x1a\ninput")) || !bytes.Equal(stored[provider.request.MaskDigest], []byte("\x89PNG\r\n\x1a\nmask")) {
		t.Fatalf("stored=%v request=%+v", stored, provider.request)
	}
}
