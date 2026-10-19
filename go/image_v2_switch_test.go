package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type switchFixture struct {
	Scope    string `json:"scope"`
	Policy   string `json:"policy"`
	Model    string `json:"model"`
	Programs struct {
		Comfy    string `json:"comfy"`
		OpenCode string `json:"opencode"`
	} `json:"programs"`
	Providers []struct {
		Name      string `json:"name"`
		BindingID string `json:"binding_id"`
		Phase     string `json:"phase"`
	} `json:"providers"`
}

type switchBackend struct {
	server   *httptest.Server
	image    []byte
	requests atomic.Int32
}

func newSwitchBackend(t *testing.T, pixel color.RGBA) *switchBackend {
	t.Helper()
	backend := &switchBackend{image: encodeSwitchPNG(t, pixel)}
	backend.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/generations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		backend.requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("local fixture received an Authorization header")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{
			"b64_json": base64.StdEncoding.EncodeToString(backend.image),
		}}})
	}))
	t.Cleanup(backend.server.Close)
	return backend
}

func encodeSwitchPNG(t *testing.T, value color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.SetRGBA(0, 0, value)
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// readSwitchFixture reads the synthetic policy and provider fixture of the
// 0.4 provider switch (research/adoption/0.4-provider-switch/RESULTS.md).
func readSwitchFixture(t *testing.T) switchFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "image-v2-switch-fixture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture switchFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Scope == "" || fixture.Policy == "" || fixture.Model == "" || len(fixture.Providers) != 2 {
		t.Fatalf("incomplete switch fixture: %+v", fixture)
	}
	return fixture
}

func absoluteFixtureProgram(value string) string {
	value = strings.ReplaceAll(value, "\\", string(filepath.Separator))
	return filepath.Join(os.TempDir(), "oa-0.4-provider-switch-fixture", value)
}

func TestImageV2ProviderSwitchUsesExactPinForBothAdopterCallers(t *testing.T) {
	fixture := readSwitchFixture(t)
	if !strings.Contains(fixture.Scope, "synthetic") {
		t.Fatalf("fixture must identify its test-double scope: %q", fixture.Scope)
	}
	programs := map[string]string{
		"comfy":    absoluteFixtureProgram(fixture.Programs.Comfy),
		"opencode": absoluteFixtureProgram(fixture.Programs.OpenCode),
	}
	for app, program := range programs {
		if !filepath.IsAbs(program) {
			t.Fatalf("%s fixture caller must be an absolute executable: %q", app, program)
		}
	}

	backends := map[string]*switchBackend{}
	hosts := make([]*router.Host, 0, len(fixture.Providers))
	for index, provider := range fixture.Providers {
		backend := newSwitchBackend(t, color.RGBA{R: uint8(40 + index*90), G: uint8(90 + index*40), B: 180, A: 255})
		backends[provider.Name] = backend
		host := router.OpenAICompatible(provider.Name, backend.server.URL, "/v1", []string{fixture.Model})
		host.BindingID = provider.BindingID
		hosts = append(hosts, host)
	}

	routes := router.New(hosts...)
	routes.Survey()
	store := &imageStore{}
	activeProvider := ""
	var decisions []string
	provider, err := New(Config{
		Router: routes,
		Decide: func(_ context.Context, subject Subject, action, resource string) (string, error) {
			if action != ActionComplete || !strings.HasPrefix(resource, "host:") {
				t.Errorf("unexpected authority check from %s: %s on %s", subject.Program, action, resource)
				return "unavailable", nil
			}
			decisions = append(decisions, subject.Program+"|"+resource)
			return "permitted", nil
		},
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		PrepareContentWrite: store.prepare,
		ResolveSelectionV2: func(_ context.Context, subject Subject, name string) (wire.SelectionPolicyV2, bool, error) {
			if name != fixture.Policy || (subject.Program != programs["comfy"] && subject.Program != programs["opencode"]) {
				t.Errorf("unexpected policy scope lookup: %+v %q", subject, name)
				return wire.SelectionPolicyV2{}, false, nil
			}
			var pin string
			for _, declared := range fixture.Providers {
				if declared.Phase == activeProvider {
					pin = declared.Name
				}
			}
			return wire.SelectionPolicyV2{Name: fixture.Policy, Candidates: []wire.SelectionCandidateV2{{
				Model: fixture.Model, Placement: wire.CandidatePlacementLocal, Provider: pin,
			}}}, pin != "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	for _, declared := range fixture.Providers {
		activeProvider = declared.Phase
		beforeRequests := make(map[string]int32, len(backends))
		for name, backend := range backends {
			beforeRequests[name] = backend.requests.Load()
		}
		store.mu.Lock()
		beforeOutputs := len(store.data)
		store.mu.Unlock()
		for _, app := range []string{"comfy", "opencode"} {
			subject := Subject{Account: "fixture-account", Program: programs[app]}
			request := imageV2Request(wire.Target{SelectionPolicy: fixture.Policy})
			request.Content.Size = "auto"
			admission := provider.StartImageV2(context.Background(), subject, request)
			if admission.Outcome != wire.StartOutcomeAccepted || admission.Host != declared.Name || admission.Model != fixture.Model ||
				admission.PreferredModel != fixture.Model || admission.SelectionReason == nil || *admission.SelectionReason != wire.SelectionReasonPreferred {
				t.Fatalf("%s phase %s admission: %+v", app, declared.Phase, admission)
			}
			_, reply := drainImage(t, provider, subject, admission.Operation)
			if reply.Outcome != wire.ReplyOutcomeCompleted || reply.Host != declared.Name || reply.Model != fixture.Model || len(reply.Deliveries) != 1 {
				t.Fatalf("%s phase %s reply: %+v", app, declared.Phase, reply)
			}
			expectedDigest := sha256.Sum256(backends[declared.Name].image)
			if reply.Deliveries[0].Digest != "sha256:"+hex.EncodeToString(expectedDigest[:]) || reply.Deliveries[0].Location != "local" {
				t.Fatalf("%s phase %s delivery does not name its pinned fixture output: %+v", app, declared.Phase, reply.Deliveries[0])
			}
			store.mu.Lock()
			index := len(store.data) - 1
			if index < beforeOutputs || store.subjects[index] != subject || !bytes.Equal(store.data[index], backends[declared.Name].image) || store.digests[index] != reply.Deliveries[0].Digest {
				store.mu.Unlock()
				t.Fatalf("%s phase %s output is not the caller-owned pinned result", app, declared.Phase)
			}
			store.mu.Unlock()
		}
		if got := backends[declared.Name].requests.Load() - beforeRequests[declared.Name]; got != 2 {
			t.Fatalf("phase %s sent %d image requests to one pinned fixture; want one from each adopter", declared.Phase, got)
		}

		// An explicitly required extension absent from this adapter is refused
		// by the selected route before it contacts either provider.
		refusal := imageV2Request(wire.Target{SelectionPolicy: fixture.Policy})
		refusal.Content.Size = "auto"
		refusal.Content.RequiredExtensions = []string{"fixture/unsupported-control"}
		refusal.Content.Extensions = map[string]string{"fixture/unsupported-control": "x"}
		got := provider.StartImageV2(context.Background(), Subject{Account: "fixture-account", Program: programs["comfy"]}, refusal)
		if got.Outcome != wire.StartOutcomeUnsupportedFeature || got.Reason != "extension:fixture/unsupported-control" {
			t.Fatalf("phase %s unsupported option: %+v", declared.Phase, got)
		}
		for name, backend := range backends {
			want := beforeRequests[name]
			if name == declared.Name {
				want += 2
			}
			if backend.requests.Load() != want {
				t.Fatalf("phase %s unsupported option reached an image endpoint", declared.Phase)
			}
		}
	}
	if len(decisions) != 4 {
		t.Fatalf("got %d host authorization checks for four requests", len(decisions))
	}
}
