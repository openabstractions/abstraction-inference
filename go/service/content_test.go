package service

import (
	"context"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	content "github.com/openabstractions/abstraction-storage/go/abstraction/storage/content"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

func TestInferenceAndContentShareAuthenticatedEndpoint(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("fixture", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	component, err := storageService.NewComponent(store, func(context.Context, *identity.Peer, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	p, err := inference.New(inference.Config{
		Router: router.New(router.NewHosted("fixture", "http://127.0.0.1:1/v1", router.WireOpenAICompatible, "")),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "denied", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	h.Content = component
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	transport := listen.FrameClient{Endpoint: endpoint}.WithDefaults(5*time.Second, MaxFrameBytes)
	digest := "sha256:" + strings.Repeat("0", 64)
	opened, err := content.NewContentReaderClient(transport).Open(digest)
	if err != nil || opened.Outcome != content.OpenOutcomeNotFound {
		t.Fatalf("content Open on inference endpoint: %+v, %v", opened, err)
	}
	start, err := wire.NewChatClient(transport).Start(wire.Request{})
	if err != nil || start.Outcome != wire.StartOutcomeInvalid {
		t.Fatalf("chat Start on same endpoint: %+v, %v", start, err)
	}
}

func TestContentStartFailureClosesInferenceEndpoint(t *testing.T) {
	// A macOS socket endpoint cannot carry Program proof; the installed
	// runtime serves inference-v1 over XPC there.
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("fixture", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	component, err := storageService.NewComponent(store, func(context.Context, *identity.Peer, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := component.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := inference.New(inference.Config{
		Router: router.New(router.NewHosted("fixture", "http://127.0.0.1:1/v1", router.WireOpenAICompatible, "")),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "denied", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	h.Content = component
	if err := h.Serve(context.Background()); err == nil {
		t.Fatal("closed content component started")
	}
	if conn, err := listen.Dial(endpoint); err == nil {
		_ = conn.Close()
		t.Fatal("inference endpoint remained open after content start failure")
	}
}
