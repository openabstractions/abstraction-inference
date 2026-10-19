package inference

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// contractMask is an [INF-I1] mask of w × h whose left quarter changes:
// transparent there, opaque white elsewhere, which a luminance reader would
// read as "change everything".
func contractMask(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/4 {
				m.SetNRGBA(x, y, color.NRGBA{})
			} else {
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// maskRoute is one image route carrying an edit mask: its host and a server
// that records the mask part it receives.
type maskRoute struct {
	provider *Provider
	mu       sync.Mutex
	masks    [][]byte
	posts    int
}

func newMaskRoute(t *testing.T, wireName string, inputs map[string][]byte) *maskRoute {
	t.Helper()
	route := &maskRoute{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"paint"}]}`)
			return
		}
		route.mu.Lock()
		route.posts++
		route.mu.Unlock()
		if err := r.ParseMultipartForm(8 << 20); err == nil {
			if files := r.MultipartForm.File["mask"]; len(files) == 1 {
				f, _ := files[0].Open()
				data, _ := io.ReadAll(f)
				f.Close()
				route.mu.Lock()
				route.masks = append(route.masks, data)
				route.mu.Unlock()
			}
		}
		if wireName == router.WireStabilityV2Beta {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngOne)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"iVBORw0KGgpmaXJzdC1pbWFnZQ=="}]}`)
	}))
	t.Cleanup(server.Close)
	var host *router.Host
	if wireName == router.WireStabilityV2Beta {
		host = router.NewHosted("stability", server.URL, router.WireStabilityV2Beta, "stability")
	} else {
		host = router.OpenAICompatible("local-images", server.URL, "/v1", []string{"paint"})
	}
	host.Profiles = []string{router.ProfileImage}
	routes := router.New(host)
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer fixture"}, nil
	})
	routes.Survey()
	store := &imageStore{}
	p, err := New(Config{Router: routes, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return map[string]string{"Authorization": "Bearer fixture"}, "applied"
		},
		PrepareContentWrite: store.prepare,
		ResolveContent: func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
			if data, ok := inputs[digest]; ok {
				return data, ContentResolved
			}
			return nil, ContentUnknown
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	route.provider = p
	return route
}

func maskRequest(model, image, mask string) wire.ImageRequest {
	req := wire.ImageRequest{Model: model, Mode: wire.ImageModeEdit, Prompt: "snow", Size: "auto", Count: 1,
		ImageDigest: image, ImageMediaType: "image/png", MaskDigest: mask, MaskMediaType: "image/png"}
	if model == "core" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, "stability"
	}
	return req
}

// [INF-I1] per route: the OpenAI-compatible route receives the alpha form,
// alpha 0 exactly on the area to change, which an sd-server behind it also
// reads by luminance; Stability's inpaint route receives a grayscale mask,
// white exactly on that area.
func TestEachRouteReceivesItsMaskForm(t *testing.T) {
	input, mask := pngHeader(64, 64), contractMask(t, 64, 64)
	inputs := map[string][]byte{digestOf(input): input, digestOf(mask): mask}
	for _, tc := range []struct {
		wire, model string
		alpha       bool
	}{{router.WireOpenAICompatible, "paint", true}, {router.WireStabilityV2Beta, "core", false}} {
		route := newMaskRoute(t, tc.wire, inputs)
		a := route.provider.StartImage(context.Background(), caller, maskRequest(tc.model, digestOf(input), digestOf(mask)))
		if a.Outcome != wire.StartOutcomeAccepted {
			t.Fatalf("%s admission %+v", tc.wire, a)
		}
		drainImage(t, route.provider, caller, a.Operation)
		route.mu.Lock()
		if len(route.masks) != 1 {
			t.Fatalf("%s received %d masks", tc.wire, len(route.masks))
		}
		decoded, err := png.Decode(bytes.NewReader(route.masks[0]))
		route.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if _, gray := decoded.(*image.Gray); gray == tc.alpha {
			t.Fatalf("%s received a %T mask", tc.wire, decoded)
		}
		for _, x := range []int{0, 15, 16, 63} {
			stored := color.NRGBAModel.Convert(decoded.At(x, 5)).(color.NRGBA)
			change := x < 16
			if luminance := stored.R; (luminance == 255) != change || (luminance == 0) == change {
				t.Fatalf("%s pixel %d luminance %d", tc.wire, x, luminance)
			}
			if tc.alpha && (stored.A == 0) != change {
				t.Fatalf("%s pixel %d alpha %d", tc.wire, x, stored.A)
			}
		}
	}
}

// [INF-I1] The native relay uploads the mask under its digest to the
// provider, which converts it, and an image engine adapter converts its own:
// both keep the contract mask byte for byte, and both still refuse another
// size.
func TestRelaysKeepTheContractMask(t *testing.T) {
	input, mask, other := pngHeader(64, 32), contractMask(t, 64, 32), contractMask(t, 32, 32)
	for _, kind := range []string{"native", "engine"} {
		inputs := map[string]resolvedImageInput{digestOf(input): {data: input, mediaType: "image/png"}, digestOf(mask): {data: mask, mediaType: "image/png"}}
		req := maskRequest("paint", digestOf(input), digestOf(mask))
		if _, why := editMask(kind, req, inputs); why != "" || !bytes.Equal(inputs[digestOf(mask)].data, mask) {
			t.Fatalf("%s: %q, mask changed %v", kind, why, !bytes.Equal(inputs[digestOf(mask)].data, mask))
		}
		inputs[digestOf(other)] = resolvedImageInput{data: other, mediaType: "image/png"}
		if outcome, why := editMask(kind, maskRequest("paint", digestOf(input), digestOf(other)), inputs); outcome != wire.StartOutcomeInvalid || why != "mask" {
			t.Fatalf("%s accepted another size: %v %q", kind, outcome, why)
		}
		// An engine adapter decodes the mask: a header without pixels and a
		// mask that changes nothing are refused. The native relay leaves the
		// decode to the receiving provider, whose own route refuses them.
		for name, bad := range map[string][]byte{"no pixels": append(pngHeader(64, 32), 0), "changes nothing": opaqueMask(t, 64, 32)} {
			inputs[digestOf(bad)] = resolvedImageInput{data: bad, mediaType: "image/png"}
			outcome, why := editMask(kind, maskRequest("paint", digestOf(input), digestOf(bad)), inputs)
			if kind == "native" && why != "" {
				t.Fatalf("native relay decoded a mask that %s: %v %q", name, outcome, why)
			}
			if kind == "engine" && (outcome != wire.StartOutcomeInvalid || why != "mask") {
				t.Fatalf("engine accepted a mask that %s: %v %q", name, outcome, why)
			}
		}
	}
}

// [INF-I1] Each route decodes the mask under its own bound: Stability's
// documented edit limit on its route, stable-diffusion.cpp's on an
// OpenAI-compatible route and an image engine; the native relay decodes
// nothing and its receiving provider applies its own.
func TestEachRouteBoundsTheMaskItDecodes(t *testing.T) {
	for _, tc := range []struct {
		kind          string
		width, height int
		within        bool
	}{
		{"stability", 3072, 3072, true}, {"stability", 3073, 3072, false}, {"stability", 64, 64, true},
		{"stability", 63, 4096, false}, {"stability", 1000, 2500, true}, {"stability", 1000, 2501, false},
		{"stability", 2501, 1000, false}, {"stability", 2304, 2304, true},
		{"openai", 2048, 2048, true}, {"openai", 2304, 2304, false}, {"openai", 32, 32, true}, {"openai", 4097, 64, false},
		{"engine", 2048, 2048, true}, {"engine", 2304, 2304, false},
	} {
		if got := maskWithinBound(tc.kind, tc.width, tc.height); got != tc.within {
			t.Errorf("%s %dx%d within=%v, want %v", tc.kind, tc.width, tc.height, got, tc.within)
		}
	}
	// A mask over stable-diffusion.cpp's bound and within Stability's: the
	// Stability route sends it, the OpenAI-compatible route refuses it.
	input, mask := pngHeader(2304, 2304), contractMask(t, 2304, 2304)
	inputs := map[string][]byte{digestOf(input): input, digestOf(mask): mask}
	stability := newMaskRoute(t, router.WireStabilityV2Beta, inputs)
	a := stability.provider.StartImage(context.Background(), caller, maskRequest("core", digestOf(input), digestOf(mask)))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("Stability refused a mask within its bound: %+v", a)
	}
	drainImage(t, stability.provider, caller, a.Operation)
	openai := newMaskRoute(t, router.WireOpenAICompatible, inputs)
	a = openai.provider.StartImage(context.Background(), caller, maskRequest("paint", digestOf(input), digestOf(mask)))
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "size:engine-bound" {
		t.Fatalf("the OpenAI-compatible route took a mask beyond sd.cpp's bound: %+v", a)
	}
	// A larger mask than Stability's bound is refused before it is decoded.
	big, bigInput := pngHeader(4096, 4096), pngHeader(4096, 4096)
	byDigest := map[string]resolvedImageInput{digestOf(bigInput): {data: bigInput, mediaType: "image/png"}, digestOf(big) + "m": {data: big, mediaType: "image/png"}}
	req := maskRequest("core", digestOf(bigInput), digestOf(big)+"m")
	if outcome, why := editMask("stability", req, byDigest); outcome != wire.StartOutcomeUnsupportedFeature || why != "size:engine-bound" {
		t.Fatalf("Stability decoded a mask beyond its bound: %v %q", outcome, why)
	}
	// The native relay takes a header of any size equal to the input's.
	if _, why := editMask("native", req, byDigest); why != "" {
		t.Fatalf("native relay bounded the mask itself: %q", why)
	}
}

// opaqueMask is a mask with no transparent pixel: it marks nothing to change.
func opaqueMask(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(m.Pix); i += 4 {
		m.Pix[i] = 255
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// [INF-I1] A mask whose pixel size differs from the input image's, or that
// does not decode, is refused invalid "mask" before any route sees it.
func TestEditMaskSizeAndDecodeAreCheckedOnceForEveryRoute(t *testing.T) {
	input := pngHeader(64, 64)
	other, broken := contractMask(t, 32, 64), pngHeader(64, 64)
	broken = append(broken, 0) // a header with no pixels behind it
	opaque := opaqueMask(t, 64, 64)
	inputs := map[string][]byte{digestOf(input): input, digestOf(other): other, digestOf(broken): broken, digestOf(opaque): opaque}
	for _, tc := range []struct{ wire, model string }{{router.WireOpenAICompatible, "paint"}, {router.WireStabilityV2Beta, "core"}} {
		route := newMaskRoute(t, tc.wire, inputs)
		for name, mask := range map[string][]byte{"other size": other, "no pixels": broken, "changes nothing": opaque} {
			a := route.provider.StartImage(context.Background(), caller, maskRequest(tc.model, digestOf(input), digestOf(mask)))
			if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "mask" {
				t.Fatalf("%s %s admission %+v", tc.wire, name, a)
			}
		}
		route.mu.Lock()
		if route.posts != 0 {
			t.Fatalf("%s received %d requests", tc.wire, route.posts)
		}
		route.mu.Unlock()
	}
}
