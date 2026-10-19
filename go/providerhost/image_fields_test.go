package providerhost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// A leaf provider over an OpenAI-compatible engine passes its ImageFields to
// inference: the declared extension a designated runtime forwards reaches the
// engine as the field the hook names.
func TestNewImageForwardsDeclaredExtensionFields(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	var mu sync.Mutex
	var bodies []map[string]any
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(examplePNG)}}})
	}))
	defer engine.Close()
	host := router.OpenAICompatible("fixture-image", engine.URL, "/api/v1", []string{"user.fixture"})
	host.Profiles = []string{router.ProfileImage}
	host.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: {"fixture/seed": true}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(t.TempDir(), "provider.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\providerhost-fields-%d-%d`, os.Getpid(), time.Now().UnixNano())
	}
	provider, err := NewImage(ImageConfig{Endpoint: endpoint, RuntimePrograms: []string{exe}, Host: host, HTTP: engine.Client(),
		StoreRoot: t.TempDir(), RecordPath: filepath.Join(t.TempDir(), "uploads.json"),
		ImageFields: func(values map[string]string) (map[string]any, string) {
			if values["fixture/seed"] != "42" {
				return nil, "fixture/seed"
			}
			return map[string]any{"seed": int64(42)}, ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := wire.NewImageClient(listen.FrameClient{Endpoint: endpoint, Timeout: 35 * time.Second, MaxFrame: 1 << 20, Sessions: true})
	a, err := client.Start(wire.ImageRequest{Model: "user.fixture", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "512x512", Count: 1,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}, Extensions: map[string]string{"fixture/seed": "42"}, RequiredExtensions: []string{"fixture/seed"}})
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v, %v", a, err)
	}
	for cursor, finished := int64(0), false; !finished; {
		page, err := client.Observe(a.Operation, cursor, 256, 65536, 20000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v, %v", page, err)
		}
		for _, delta := range page.Deltas {
			if delta.ImageEnd != nil {
				finished = true
				if delta.ImageEnd.Outcome != wire.ReplyOutcomeCompleted {
					t.Fatalf("end %+v", delta.ImageEnd)
				}
			}
		}
		if page.AtEnd && !finished {
			t.Fatalf("missing end %+v", page)
		}
		cursor = page.Next
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || bodies[0]["seed"] != float64(42) {
		t.Fatalf("engine bodies %v", bodies)
	}
}
