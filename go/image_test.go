package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

var (
	pngOne = []byte("\x89PNG\r\n\x1a\nfirst-image")
	pngTwo = []byte("\x89PNG\r\n\x1a\nsecond-image")
)

type imageStore struct {
	mu       sync.Mutex
	subjects []Subject
	data     [][]byte
	digests  []string
	deny     bool
}

func (s *imageStore) prepare(_ context.Context, subject Subject, profile, media string, max int64) (ContentCommitter, ContentOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deny {
		return nil, ContentForbidden
	}
	if profile != "image" || media != "image/*" || max < 1 {
		return nil, ContentUnavailable
	}
	index := len(s.data)
	s.subjects = append(s.subjects, subject)
	s.data = append(s.data, nil)
	s.digests = append(s.digests, "")
	return func(_ context.Context, digest string, data []byte) ContentOutcome {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.data[index] = append([]byte(nil), data...)
		s.digests[index] = digest
		return ContentResolved
	}, ContentResolved
}

type imageFixture struct {
	provider *Provider
	server   *httptest.Server
	store    *imageStore
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
}

func newImageFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request, []byte)) *imageFixture {
	t.Helper()
	f := &imageFixture{store: &imageStore{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(context.Background()))
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-image-1"}]}`)
			return
		}
		handler(w, r, body)
	}))
	t.Cleanup(f.server.Close)
	host := router.NewHosted("openai", f.server.URL+"/v1", router.WireOpenAICompatible, "openai")
	routes := router.New(host)
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer fixture"}, nil
	})
	routes.Survey()
	p, err := New(Config{Router: routes, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return map[string]string{"Authorization": "Bearer fixture"}, "applied"
		}, PrepareContentWrite: f.store.prepare})
	if err != nil {
		t.Fatal(err)
	}
	f.provider = p
	t.Cleanup(func() { _ = p.Close() })
	return f
}

func imageRequestFixture(mode wire.ImageMode, count int64) wire.ImageRequest {
	return wire.ImageRequest{Model: "gpt-image-1", Mode: mode, Prompt: "a small lighthouse", Size: "1024x1024", Count: count,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "openai", Extensions: map[string]string{}}
}

func imageTestBounds() imageOutputBounds { return imageOutputBounds{each: 20 << 20, total: 64 << 20} }

func drainImage(t *testing.T, p *Provider, subject Subject, id string) ([]wire.Delta, wire.ImageReply) {
	t.Helper()
	var deltas []wire.Delta
	for cursor, deadline := int64(0), time.Now().Add(5*time.Second); time.Now().Before(deadline); {
		page := p.ObserveImage(context.Background(), subject, id, cursor, 256, 65536, 500)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v", page)
		}
		for _, delta := range page.Deltas {
			if delta.Sequence != cursor {
				t.Fatalf("sequence %d cursor %d", delta.Sequence, cursor)
			}
			cursor++
			deltas = append(deltas, delta)
			if delta.ImageEnd != nil {
				return deltas, *delta.ImageEnd
			}
		}
	}
	t.Fatal("image operation did not end")
	return nil, wire.ImageReply{}
}

func TestImageProgressAndResultsMatchCommittedDigests(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer fixture" || !bytes.Contains(body, []byte(`"response_format":"b64_json"`)) {
			t.Errorf("request %s auth=%q body=%s", r.URL.Path, r.Header.Get("Authorization"), body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}, {"b64_json": base64.StdEncoding.EncodeToString(pngTwo)}}})
	})
	a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 2))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	deltas, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Images != 2 || len(end.Deliveries) != 2 {
		t.Fatalf("end %+v", end)
	}
	progress, results := 0, 0
	for _, delta := range deltas {
		if delta.ImageProgress != nil {
			progress++
		}
		if delta.ImageResult != nil {
			if delta.ImageResult.Index != int64(results) || delta.ImageResult.Delivery != end.Deliveries[results] {
				t.Fatalf("result %+v", delta.ImageResult)
			}
			results++
		}
	}
	if progress == 0 || results != 2 {
		t.Fatalf("progress=%d results=%d", progress, results)
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	for i, data := range [][]byte{pngOne, pngTwo} {
		sum := sha256.Sum256(data)
		want := "sha256:" + hex.EncodeToString(sum[:])
		if f.store.subjects[i] != caller || !bytes.Equal(f.store.data[i], data) || f.store.digests[i] != want || end.Deliveries[i].Digest != want {
			t.Fatalf("stored %d digest=%q delivery=%+v", i, f.store.digests[i], end.Deliveries[i])
		}
	}
}

func TestImageEditSendsAuthorizedImageAndMask(t *testing.T) {
	image, mask := pngOne, pngTwo
	imageSum, maskSum := sha256.Sum256(image), sha256.Sum256(mask)
	imageDigest, maskDigest := "sha256:"+hex.EncodeToString(imageSum[:]), "sha256:"+hex.EncodeToString(maskSum[:])
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.URL.Path != "/v1/images/edits" {
			t.Errorf("path %s", r.URL.Path)
		}
		media, params, _ := mimeParse(r.Header.Get("Content-Type"))
		if media != "multipart/form-data" {
			t.Errorf("content type %q", media)
			return
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		seen := map[string][]byte{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			seen[part.FormName()], _ = io.ReadAll(part)
		}
		if !bytes.Equal(seen["image"], image) || !bytes.Equal(seen["mask"], mask) {
			t.Errorf("multipart image=%d mask=%d", len(seen["image"]), len(seen["mask"]))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	f.provider.cfg.ResolveContent = func(_ context.Context, subject Subject, digest string, _ int64) ([]byte, ContentOutcome) {
		if subject != caller {
			return nil, ContentForbidden
		}
		switch digest {
		case imageDigest:
			return image, ContentResolved
		case maskDigest:
			return mask, ContentResolved
		}
		return nil, ContentUnknown
	}
	req := imageRequestFixture(wire.ImageModeEdit, 1)
	req.ImageDigest, req.ImageMediaType, req.MaskDigest, req.MaskMediaType = imageDigest, "image/png", maskDigest, "image/png"
	a := f.provider.StartImage(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
}

func TestImageOutputDenialPrecedesUpstream(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected paid request %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	f.store.deny = true
	a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeForbidden || a.Reason != "output:forbidden" {
		t.Fatalf("admission %+v", a)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if request.Method == http.MethodPost {
			t.Fatalf("paid request %s", request.URL.Path)
		}
	}
}

func TestImageAdapterRefusesUnsupportedSizeBeforeUpstream(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected paid request %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	req := imageRequestFixture(wire.ImageModeGenerate, 1)
	req.Size = "640x640"
	a := f.provider.StartImage(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "size:640x640" {
		t.Fatalf("admission %+v", a)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if request.Method == http.MethodPost {
			t.Fatalf("paid request %s", request.URL.Path)
		}
	}
}

func TestImageLocalDeliveryRequirementPermitsHostedProvider(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	a := f.provider.StartImage(WithLocalImageDelivery(context.Background()), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || len(end.Deliveries) != 1 || end.Deliveries[0].Location != "local" {
		t.Fatalf("end %+v", end)
	}
}

func TestRemoteImageValidationRejectsMalformedPagesAndFailedDeliveries(t *testing.T) {
	delivery := wire.Delivery{Digest: "sha256:" + strings.Repeat("1", 64), MediaType: "image/png", Size: 10, Location: "local"}
	if reason := validateRemoteImageTerminal(imageRequestFixture(wire.ImageModeGenerate, 1), nil, wire.ImageReply{
		Outcome: wire.ReplyOutcomeUnavailable, Deliveries: []wire.Delivery{delivery}, Images: 1,
	}); reason != "remote:terminal" {
		t.Fatalf("failed terminal reason = %q", reason)
	}
	if reason := validateRemoteImagePage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Next: 0, Deltas: []wire.Delta{{Sequence: 0}}}, 0); reason != "remote:page" {
		t.Fatalf("unchanged cursor reason = %q", reason)
	}
	end := &wire.ImageReply{Outcome: wire.ReplyOutcomeCompleted, Deliveries: []wire.Delivery{delivery}, Images: 1}
	if reason := validateRemoteImagePage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Next: 1, Deltas: []wire.Delta{{Sequence: 0, Kind: wire.DeltaKindEnd, ImageEnd: end}}}, 0); reason != "remote:terminal" {
		t.Fatalf("unterminated end page reason = %q", reason)
	}
	if reason := remoteImageDeltaShape(wire.Delta{Kind: wire.DeltaKindImageResult, ImageResult: &wire.ImageResult{}, ImageProgress: &wire.ImageProgress{}}); reason != "remote:page" {
		t.Fatalf("multi-field delta reason = %q", reason)
	}
}

func TestRemoteImagePageEnforcesRequestedCount(t *testing.T) {
	deltas := make([]wire.Delta, 257)
	for i := range deltas {
		deltas[i].Sequence = int64(i)
	}
	if reason := validateRemoteImagePage(wire.DeltaPage{Next: 257, Deltas: deltas}, 0); reason != "remote:page" {
		t.Fatalf("oversized page accepted: %q", reason)
	}
}

func TestImageInputDenialPrecedesOutputAndUpstream(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected paid request %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	digest := "sha256:" + strings.Repeat("1", 64)
	f.provider.cfg.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) { return nil, ContentForbidden }
	req := imageRequestFixture(wire.ImageModeEdit, 1)
	req.ImageDigest, req.ImageMediaType = digest, "image/png"
	a := f.provider.StartImage(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeForbidden || a.Reason != "content:read:forbidden" {
		t.Fatalf("admission %+v", a)
	}
	f.store.mu.Lock()
	prepared := len(f.store.data)
	f.store.mu.Unlock()
	if prepared != 0 {
		t.Fatalf("prepared %d outputs after denied input", prepared)
	}
}

func TestImageCeilingCountsGeneratedImages(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}, {"b64_json": base64.StdEncoding.EncodeToString(pngTwo)}}})
	})
	ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"openai": {ImagesPerDay: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.cfg.Ceilings = ceilings
	a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 2))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("first %+v", a)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Images != 2 {
		t.Fatalf("end %+v", end)
	}
	a = f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeBudgetExceeded || a.Reason != "ceiling:images:openai" {
		t.Fatalf("second %+v", a)
	}
}

func TestImageOperationIsProfileIsolated(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if got := f.provider.Observe(context.Background(), caller, a.Operation, 0, 1, 65536, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("cross observe %+v", got)
	}
	if got := f.provider.Cancel(caller, a.Operation); got.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("cross cancel %+v", got)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
}

func mimeParse(value string) (string, map[string]string, error) {
	return mime.ParseMediaType(value)
}

type imageRoundTrip func(*http.Request) (*http.Response, error)

func (fn imageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func imageResponse(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	for key, value := range headers {
		h.Set(key, value)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestReplicatePollingHonorsRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var submitted time.Time
	client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.String())
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/predictions"):
			submitted = time.Now()
			return imageResponse(http.StatusCreated, `{"id":"p1","status":"starting"}`, map[string]string{"Retry-After": "0"}), nil
		case r.Method == http.MethodGet && r.URL.Host == "api.replicate.com":
			if time.Since(submitted) > 80*time.Millisecond {
				t.Errorf("Retry-After 0 was replaced by default delay")
			}
			return imageResponse(http.StatusOK, `{"id":"p1","status":"succeeded","output":["https://replicate.delivery/out.png"]}`, nil), nil
		case r.Method == http.MethodGet && r.URL.Host == "replicate.delivery":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(pngOne))}, nil
		}
		return imageResponse(http.StatusNotFound, `{}`, nil), nil
	})}
	host := router.NewHosted("replicate", "https://api.replicate.com", router.WireReplicatePredictions, "replicate")
	reply := runReplicateImage(context.Background(), client, host, "owner/model", imageRequestFixture(wire.ImageModeGenerate, 1), map[string]string{"Authorization": "Bearer secret"}, imageTestBounds(), func(int64, string) {})
	if reply.outcome != wire.ReplyOutcomeCompleted || len(reply.images) != 1 || !bytes.Equal(reply.images[0].data, pngOne) {
		t.Fatalf("reply %+v calls=%v", reply, calls)
	}
}

func TestImagePollingClampsZeroRetryAfter(t *testing.T) {
	resp := imageResponse(http.StatusOK, `{}`, map[string]string{"Retry-After": "0"})
	defer resp.Body.Close()
	if delay := retryDelay(resp); delay < 25*time.Millisecond {
		t.Fatalf("zero Retry-After delay = %v, want bounded minimum", delay)
	}
}

func TestImagePollingHasTotalBudget(t *testing.T) {
	ctx, cancel := imagePollContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= maximumImagePollTime-time.Second || remaining > maximumImagePollTime {
		t.Fatalf("poll budget deadline ok=%v remaining=%v", ok, remaining)
	}
}

func TestQueuedImagePollFailureCancelsKnownJob(t *testing.T) {
	tests := []struct {
		name      string
		run       func(context.Context, *http.Client, *router.Host) imageBackendReply
		host      *router.Host
		roundTrip func(*http.Request, *int) *http.Response
	}{
		{
			name: "replicate",
			host: router.NewHosted("replicate", "https://api.replicate.com", router.WireReplicatePredictions, "replicate"),
			run: func(ctx context.Context, client *http.Client, host *router.Host) imageBackendReply {
				return runReplicateImage(ctx, client, host, "owner/model", imageRequestFixture(wire.ImageModeGenerate, 1), map[string]string{}, imageTestBounds(), func(int64, string) {})
			},
			roundTrip: func(r *http.Request, cancels *int) *http.Response {
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
					*cancels++
					return imageResponse(http.StatusOK, `{}`, nil)
				case r.Method == http.MethodPost:
					return imageResponse(http.StatusCreated, `{"id":"p1","status":"starting"}`, map[string]string{"Retry-After": "0"})
				default:
					return imageResponse(http.StatusInternalServerError, `{}`, nil)
				}
			},
		},
		{
			name: "fal",
			host: router.NewHosted("fal", "https://queue.fal.run", router.WireFalQueue, "fal"),
			run: func(ctx context.Context, client *http.Client, host *router.Host) imageBackendReply {
				return runFalImage(ctx, client, host, "owner/model", imageRequestFixture(wire.ImageModeGenerate, 1), nil, imageTestBounds(), func(int64, string) {})
			},
			roundTrip: func(r *http.Request, cancels *int) *http.Response {
				switch {
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/cancel"):
					*cancels++
					return imageResponse(http.StatusOK, `{}`, nil)
				case r.Method == http.MethodPost:
					return imageResponse(http.StatusCreated, `{"request_id":"p1","status":"IN_QUEUE"}`, map[string]string{"Retry-After": "0"})
				default:
					return imageResponse(http.StatusInternalServerError, `{}`, nil)
				}
			},
		},
		{
			name: "comfyui",
			host: router.ComfyUI("http://127.0.0.1:8188"),
			run: func(ctx context.Context, client *http.Client, host *router.Host) imageBackendReply {
				return runComfyImage(ctx, client, host, "model.safetensors", wire.ImageRequest{Prompt: "test", Size: "512x512", Count: 1, Extensions: map[string]string{}}, imageTestBounds(), func(int64, string) {})
			},
			roundTrip: func(r *http.Request, cancels *int) *http.Response {
				switch r.URL.Path {
				case "/prompt":
					return imageResponse(http.StatusOK, `{"prompt_id":"p1"}`, map[string]string{"Retry-After": "0"})
				case "/queue":
					*cancels++
					return imageResponse(http.StatusOK, `{}`, nil)
				default:
					return imageResponse(http.StatusInternalServerError, `{}`, nil)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cancels := 0
			client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
				return test.roundTrip(r, &cancels), nil
			})}
			reply := test.run(context.Background(), client, test.host)
			if reply.outcome != wire.ReplyOutcomeUnavailable || cancels != 1 {
				t.Fatalf("reply=%+v cancels=%d", reply, cancels)
			}
		})
	}
}

func TestGeneratedImageRedirectIsRevalidated(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return imageResponse(http.StatusFound, `{}`, map[string]string{"Location": "http://127.0.0.1/private.png"}), nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pngOne))}, nil
	})}
	if _, ok := fetchGeneratedImage(context.Background(), client, "https://replicate.delivery/out.png", "replicate", 20<<20); ok || requests != 1 {
		t.Fatalf("redirect ok=%v requests=%d", ok, requests)
	}
	if client.CheckRedirect != nil {
		t.Fatal("shared HTTP redirect policy was mutated")
	}
}

func TestReplicateRefusesOutputCountBeforeDownloads(t *testing.T) {
	downloads := 0
	client := &http.Client{Transport: imageRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return imageResponse(http.StatusCreated, `{"id":"p1","status":"succeeded","output":["https://replicate.delivery/1.png","https://replicate.delivery/2.png"]}`, nil), nil
		}
		downloads++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pngOne))}, nil
	})}
	reply := runReplicateImage(context.Background(), client, router.NewHosted("replicate", "https://api.replicate.com", router.WireReplicatePredictions, "replicate"), "owner/model", imageRequestFixture(wire.ImageModeGenerate, 1), map[string]string{}, imageTestBounds(), func(int64, string) {})
	if reply.outcome != wire.ReplyOutcomeUnavailable || reply.reason != "upstream:count" || downloads != 0 {
		t.Fatalf("reply=%+v downloads=%d", reply, downloads)
	}
}

func TestQueuedImagesStopBeforeAggregateLimitDownload(t *testing.T) {
	downloads := 0
	client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pngOne))}, nil
	})}
	reply := fetchQueuedImages(context.Background(), client,
		[]string{"https://replicate.delivery/1.png", "https://replicate.delivery/2.png"}, 2, "replicate",
		imageOutputBounds{each: int64(len(pngOne)), total: int64(len(pngOne))})
	if reply.outcome != wire.ReplyOutcomeUnavailable || downloads != 1 {
		t.Fatalf("reply=%+v downloads=%d", reply, downloads)
	}
}

func TestComfyWorkflowRunsPromptHistoryAndView(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/prompt":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["prompt"] == nil {
				t.Error("workflow missing")
			}
			_, _ = io.WriteString(w, `{"prompt_id":"p1"}`)
		case "/history/p1":
			_, _ = io.WriteString(w, `{"p1":{"status":{"completed":true,"status_str":"success"},"outputs":{"7":{"images":[{"filename":"out.png","subfolder":"","type":"output"}]}}}}`)
		case "/view":
			_, _ = w.Write(pngOne)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host := router.ComfyUI(server.URL)
	reply := runComfyImage(context.Background(), server.Client(), host, "model.safetensors", wire.ImageRequest{Prompt: "test", Size: "512x512", Count: 1, Extensions: map[string]string{}}, imageTestBounds(), func(int64, string) {})
	if reply.outcome != wire.ReplyOutcomeCompleted || len(reply.images) != 1 {
		t.Fatalf("reply %+v", reply)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(paths, ",") != "/prompt,/history/p1,/view" {
		t.Fatalf("paths %v", paths)
	}
}

func TestComfyCancellationDeletesQueuedPrompt(t *testing.T) {
	historyStarted := make(chan struct{})
	deleted := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			w.Header().Set("Retry-After", "0")
			_, _ = io.WriteString(w, `{"prompt_id":"p1"}`)
		case "/history/p1":
			close(historyStarted)
			<-r.Context().Done()
		case "/queue":
			var body struct {
				Delete []string `json:"delete"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			deleted <- body.Delete
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan imageBackendReply, 1)
	go func() {
		done <- runComfyImage(ctx, server.Client(), router.ComfyUI(server.URL), "model.safetensors", wire.ImageRequest{Prompt: "test", Size: "512x512", Count: 1, Extensions: map[string]string{}}, imageTestBounds(), func(int64, string) {})
	}()
	select {
	case <-historyStarted:
	case <-time.After(time.Second):
		t.Fatal("history poll did not start")
	}
	cancel()
	select {
	case reply := <-done:
		if reply.outcome != wire.ReplyOutcomeCancelled {
			t.Fatalf("reply %+v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not stop")
	}
	select {
	case ids := <-deleted:
		if len(ids) != 1 || ids[0] != "p1" {
			t.Fatalf("deleted %v", ids)
		}
	case <-time.After(time.Second):
		t.Fatal("queued prompt was not deleted")
	}
}
