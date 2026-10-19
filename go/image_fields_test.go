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
	"strconv"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type fieldsCapture struct {
	mu       sync.Mutex
	types    []string
	bodies   [][]byte
	received []map[string]string
}

// imageFieldsFixture is a provider-local OpenAI-compatible image host, as a
// leaf provider composes one, that declares lemonade/seed and maps it through
// fields. The host answers every request with one PNG.
func imageFieldsFixture(t *testing.T, declare bool, fields func(map[string]string) (map[string]any, string)) (*Provider, *fieldsCapture) {
	t.Helper()
	capture := &fieldsCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capture.mu.Lock()
		capture.types = append(capture.types, r.Header.Get("Content-Type"))
		capture.bodies = append(capture.bodies, body)
		capture.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	}))
	t.Cleanup(server.Close)
	host := router.OpenAICompatible("lemonade-image", server.URL, "/api/v1", []string{"user.Z-Image-Turbo"})
	host.Profiles = []string{router.ProfileImage}
	if declare {
		host.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: {"lemonade/seed": true}}
	}
	routes := router.New(host)
	routes.Survey()
	store := &imageStore{}
	cfg := Config{Router: routes, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: store.prepare,
		ResolveContent: func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
			return pngHeader(64, 64), ContentResolved
		},
	}
	if fields != nil {
		cfg.ImageFieldsHost = host
		cfg.ImageFields = func(values map[string]string) (map[string]any, string) {
			capture.mu.Lock()
			capture.received = append(capture.received, values)
			capture.mu.Unlock()
			return fields(values)
		}
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, capture
}

func seedFields(values map[string]string) (map[string]any, string) {
	n, err := strconv.ParseInt(values["lemonade/seed"], 10, 64)
	if err != nil {
		return nil, "lemonade/seed"
	}
	return map[string]any{"seed": n}, ""
}

func pngDigest() string {
	sum := sha256.Sum256(pngHeader(64, 64))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// A leaf provider forwards the vendor extensions its host declares as typed
// fields of the generation JSON and of the edit form; an undeclared extension
// never reaches its hook, and a required declared one is admitted.
func TestImageFieldsCarryDeclaredExtensionsToTheHost(t *testing.T) {
	p, capture := imageFieldsFixture(t, true, seedFields)
	generate := wire.ImageRequest{Model: "user.Z-Image-Turbo", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "1024x1024", Count: 1,
		Extensions: map[string]string{"lemonade/seed": "20261001", "lemonade/steps": "50"}, RequiredExtensions: []string{"lemonade/seed"}}
	a := p.StartImage(context.Background(), caller, generate)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("generate admission %+v", a)
	}
	if _, end := drainImage(t, p, caller, a.Operation); end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("generate end %+v", end)
	}
	edit := wire.ImageRequest{Model: "user.Z-Image-Turbo", Mode: wire.ImageModeEdit, Prompt: "snow", Size: "1024x1024", Count: 1,
		ImageDigest: pngDigest(), ImageMediaType: "image/png", Extensions: map[string]string{"lemonade/seed": "7"}}
	a = p.StartImage(context.Background(), caller, edit)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("edit admission %+v", a)
	}
	if _, end := drainImage(t, p, caller, a.Operation); end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("edit end %+v", end)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.received) != 2 || len(capture.received[0]) != 1 || capture.received[0]["lemonade/seed"] != "20261001" {
		t.Fatalf("hook received %v", capture.received)
	}
	dec := json.NewDecoder(bytes.NewReader(capture.bodies[0]))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["seed"] != json.Number("20261001") || body["prompt"] != "a lighthouse" {
		t.Fatalf("generation body %s", capture.bodies[0])
	}
	if _, ok := body["lemonade/steps"]; ok {
		t.Fatalf("undeclared extension forwarded: %s", capture.bodies[0])
	}
	_, params, err := mime.ParseMediaType(capture.types[1])
	if err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(bytes.NewReader(capture.bodies[1]), params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if form.Value["seed"][0] != "7" || len(form.File["image"]) != 1 {
		t.Fatalf("edit form %v", form.Value)
	}
}

// A value the hook refuses, or a field that would replace one inference sets,
// ends the call before any request reaches the host. A host that declares an
// extension without a hook to carry it refuses that extension when required.
func TestImageFieldsRefusalsReachNoHost(t *testing.T) {
	request := func(value string, required bool) wire.ImageRequest {
		r := wire.ImageRequest{Model: "user.Z-Image-Turbo", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "512x512", Count: 1,
			Extensions: map[string]string{"lemonade/seed": value}}
		if required {
			r.RequiredExtensions = []string{"lemonade/seed"}
		}
		return r
	}
	p, capture := imageFieldsFixture(t, true, seedFields)
	if a := p.StartImage(context.Background(), caller, request("forty-two", false)); a.Outcome != wire.StartOutcomeInvalid || a.Reason != "extension:lemonade/seed" {
		t.Fatalf("refused value admission %+v", a)
	}
	collide, _ := imageFieldsFixture(t, true, func(map[string]string) (map[string]any, string) {
		return map[string]any{"prompt": "replaced"}, ""
	})
	if a := collide.StartImage(context.Background(), caller, request("1", false)); a.Outcome != wire.StartOutcomeInvalid || a.Reason != "extension:fields" {
		t.Fatalf("colliding field admission %+v", a)
	}
	unhooked, unhookedCapture := imageFieldsFixture(t, true, nil)
	if a := unhooked.StartImage(context.Background(), caller, request("1", true)); a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "extension:lemonade/seed" {
		t.Fatalf("unhooked required admission %+v", a)
	}
	undeclared, undeclaredCapture := imageFieldsFixture(t, false, seedFields)
	if a := undeclared.StartImage(context.Background(), caller, request("1", true)); a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "extension:lemonade/seed" {
		t.Fatalf("undeclared required admission %+v", a)
	}
	for _, c := range []*fieldsCapture{capture, unhookedCapture, undeclaredCapture} {
		c.mu.Lock()
		if len(c.bodies) != 0 {
			t.Fatalf("a refused call reached the host: %s", c.bodies)
		}
		c.mu.Unlock()
	}
}
