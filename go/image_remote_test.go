package inference_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	identityremote "github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

type imageRemoteInventory struct{}

func (imageRemoteInventory) Models(bool) (routerwire.ModelsSnapshot, error) {
	return routerwire.ModelsSnapshot{Models: []routerwire.Family{{Family: "gpt-image-1", Names: []routerwire.Alias{{Name: "gpt-image-1", Servable: true, Hosted: true}}}}}, nil
}
func (imageRemoteInventory) Hosts(bool) (routerwire.HostsSnapshot, error) {
	return routerwire.HostsSnapshot{}, nil
}
func (imageRemoteInventory) Pick(routerwire.PickRequest) (routerwire.PickResult, error) {
	return routerwire.PickResult{}, nil
}

type countingRemoteImage struct{ starts atomic.Int64 }

func (r *countingRemoteImage) Start(wire.ImageRequest) (wire.Admission, error) {
	r.starts.Add(1)
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "unexpected"}, nil
}
func (*countingRemoteImage) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (*countingRemoteImage) Cancel(string) (wire.Cancellation, error) {
	return wire.Cancellation{}, nil
}

func TestImageLocalDeliveryRequirementRefusesRemoteBeforeStart(t *testing.T) {
	serverTLS, clientTLS := remoteLiveTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteImage := &countingRemoteImage{}
	handler := func(_ context.Context, _ identityremote.Peer, frame []byte) ([]byte, error) {
		return wire.ServeEndpoint(frame, "remote-test", "", &routerwire.RouterDispatcher{Handler: imageRemoteInventory{}}, &wire.ImageDispatcher{Handler: remoteImage})
	}
	serveCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (identityremote.Server{TLS: serverTLS, Handler: handler, Timeout: 5 * time.Second, MaxFrame: 8 << 20}).Serve(serveCtx, listener)
	}()
	t.Cleanup(func() {
		stop()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("remote server: %v", err)
		}
	})

	host, err := router.NewRemote("lab", listener.Addr().String(), clientTLS, "remote-key")
	if err != nil {
		t.Fatal(err)
	}
	host.Profiles = []string{router.ProfileImage}
	routes := router.New(host)
	routes.Survey()
	decisions := atomic.Int64{}
	provider, err := inference.New(inference.Config{Router: routes, SurveyAge: time.Hour, Decide: func(context.Context, inference.Subject, string, string) (string, error) {
		decisions.Add(1)
		return "permitted", nil
	}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
		return nil, "not_permitted"
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	req := wire.ImageRequest{Model: "gpt-image-1", Mode: wire.ImageModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "remote-key", Extensions: map[string]string{}}
	got := provider.StartImage(inference.WithLocalImageDelivery(context.Background()), inference.Subject{Account: "account", Program: "/apps/images"}, req)
	if got.Outcome != wire.StartOutcomeUnsupportedFeature || got.Reason != "delivery:remote" {
		t.Fatalf("admission %+v", got)
	}
	if remoteImage.starts.Load() != 0 || decisions.Load() != 0 {
		t.Fatalf("remote starts=%d rights decisions=%d", remoteImage.starts.Load(), decisions.Load())
	}
}
