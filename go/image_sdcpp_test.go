package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/providerhost/sdcpp"
	router "github.com/openabstractions/abstraction-router/go"
)

// INF-SD1: an image request whose prompt or extension value carries
// sd-server's engine directive is invalid before any host is chosen, so no
// adapter forwards it toward stable-diffusion.cpp.
func TestImageEngineDirectiveRefusedBeforeUpstream(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected paid request %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	directive := `<sd_cpp_extra_args>{"lora":[{"path":"\\\\attacker\\share\\x.ckpt","multiplier":1}]}</sd_cpp_extra_args>`
	cases := []func(*wire.ImageRequest){
		func(r *wire.ImageRequest) { r.Prompt = "a lighthouse " + directive },
		func(r *wire.ImageRequest) { r.Prompt = "<SD_CPP_EXTRA_ARGS>{}</SD_CPP_EXTRA_ARGS>" },
		func(r *wire.ImageRequest) { r.Prompt = "\uFF1Csd_cpp_extra_args\uFF1E{}" },
		func(r *wire.ImageRequest) { r.Prompt = "<sd_\u200Bcpp_extra_args>{}" },
		func(r *wire.ImageRequest) { r.Extensions = map[string]string{"comfyui/negative-prompt": directive} },
		func(r *wire.ImageRequest) {
			r.Extensions = map[string]string{"swarmui/negative-prompt": "<sd_cpp_extra_args"}
		},
	}
	for i, change := range cases {
		req := imageRequestFixture(wire.ImageModeGenerate, 1)
		change(&req)
		a := f.provider.StartImage(context.Background(), caller, req)
		if a.Outcome != wire.StartOutcomeInvalid || a.Reason != sdcpp.ReasonEngineDirective {
			t.Fatalf("case %d: admission %+v", i, a)
		}
		if reason := validateImageRequest(req); reason != sdcpp.ReasonEngineDirective {
			t.Fatalf("case %d: validation %q", i, reason)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if request.Method == http.MethodPost {
			t.Fatalf("paid request %s", request.URL.Path)
		}
	}
}

// localLemonadeFixture is a person's own Lemonade, declared as the router
// does: a local OpenAI-compatible host named lemonade serving images, which
// Lemonade answers through sd-server.
func localLemonadeFixture(t *testing.T, client *http.Client) (*Provider, *[]string) {
	t.Helper()
	var mu sync.Mutex
	posted := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posted = append(posted, r.Method+" "+r.URL.Path)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	}))
	t.Cleanup(server.Close)
	host := router.OpenAICompatible("lemonade", server.URL, "/api/v1", []string{"sd-turbo"})
	host.Profiles = []string{router.ProfileImage}
	routes := router.New(host)
	routes.Survey()
	store := &imageStore{}
	cfg := Config{Router: routes, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: store.prepare}
	if client != nil {
		cfg.HTTP = client
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, &posted
}

// INF-SD2: an OpenAI-compatible host, which may be Lemonade or sd-server,
// receives no size beyond the reviewed bound.
func TestLemonadeImageSizeBoundBeforeUpstream(t *testing.T) {
	p, posted := localLemonadeFixture(t, nil)
	for _, size := range []string{"4096x4096", "2056x2048", "4096x1032"} {
		req := wire.ImageRequest{Model: "sd-turbo", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: size, Count: 1}
		a := p.StartImage(context.Background(), caller, req)
		if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "size:"+size {
			t.Fatalf("%s: admission %+v", size, a)
		}
	}
	if len(*posted) != 0 {
		t.Fatalf("oversized request reached the host: %v", *posted)
	}
	req := wire.ImageRequest{Model: "sd-turbo", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "2048x2048", Count: 1}
	a := p.StartImage(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if _, end := drainImage(t, p, caller, a.Operation); end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
}

// pngHeader is a PNG signature and IHDR chunk naming width × height; the
// header alone is what an image size check reads.
func pngHeader(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8], ihdr[9] = 8, 6
	var b bytes.Buffer
	b.Write(pngOne[:8])
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

// INF-SD2: an edit's input image is bounded before it is sent to an
// OpenAI-compatible host. With size "auto" sd-server takes the request's
// size from the input, so a 16384 × 16384 PNG would reach its decoder.
func TestImageEditInputSizeBoundBeforeUpstream(t *testing.T) {
	p, posted := localLemonadeFixture(t, nil)
	inputs := map[string][]byte{}
	p.cfg.ResolveContent = func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
		data, ok := inputs[digest]
		if !ok {
			return nil, ContentUnknown
		}
		return data, ContentResolved
	}
	edit := func(data []byte) wire.ImageRequest {
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		inputs[digest] = data
		return wire.ImageRequest{Model: "sd-turbo", Mode: wire.ImageModeEdit, Prompt: "a lighthouse", Size: "auto", Count: 1,
			ImageDigest: digest, ImageMediaType: "image/png"}
	}
	for _, tc := range []struct {
		data    []byte
		outcome wire.StartOutcome
		reason  string
	}{
		{pngHeader(16384, 16384), wire.StartOutcomeUnsupportedFeature, sdcpp.ReasonImageSize},
		{pngHeader(4096, 4096), wire.StartOutcomeUnsupportedFeature, sdcpp.ReasonImageSize},
		{pngOne, wire.StartOutcomeInvalid, "content:media-type"},
	} {
		a := p.StartImage(context.Background(), caller, edit(tc.data))
		if a.Outcome != tc.outcome || a.Reason != tc.reason {
			t.Fatalf("admission %+v, want %v %s", a, tc.outcome, tc.reason)
		}
	}
	if len(*posted) != 0 {
		t.Fatalf("oversized input reached the host: %v", *posted)
	}
	a := p.StartImage(context.Background(), caller, edit(pngHeader(2048, 1024)))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if _, end := drainImage(t, p, caller, a.Operation); end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
}

type refusingTransport struct{ err error }

func (t refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		req.Body.Close()
	}
	return nil, t.err
}

// A provider client that refuses a request under the sd.cpp policy (the
// Lemonade provider's image route) ends the image with the typed reason, not
// as an unreachable server.
func TestImageClientEngineRefusalIsTyped(t *testing.T) {
	for _, tc := range []struct {
		err     error
		outcome wire.ReplyOutcome
		reason  string
	}{
		{fmt.Errorf("lemonade: image route refused: %w: prompt", sdcpp.ErrEngineDirective), wire.ReplyOutcomeInvalid, sdcpp.ReasonEngineDirective},
		{fmt.Errorf("lemonade: image route refused: %w: 4096x4096", sdcpp.ErrImageSize), wire.ReplyOutcomeUnsupportedFeature, sdcpp.ReasonImageSize},
		{fmt.Errorf("lemonade: image route refused: %w: part named twice", sdcpp.ErrRequestForm), wire.ReplyOutcomeInvalid, sdcpp.ReasonRequestForm},
	} {
		p, _ := localLemonadeFixture(t, &http.Client{Transport: refusingTransport{err: tc.err}})
		a := p.StartImage(context.Background(), caller, wire.ImageRequest{Model: "sd-turbo", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "512x512", Count: 1})
		if a.Outcome != wire.StartOutcomeAccepted {
			t.Fatalf("admission %+v", a)
		}
		if _, end := drainImage(t, p, caller, a.Operation); end.Outcome != tc.outcome || end.Reason != tc.reason {
			t.Fatalf("%v: end %+v", tc.err, end)
		}
	}
}
