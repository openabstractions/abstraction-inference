package inference_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	identityremote "github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

// This TLS fixture stays in core so remote transport tests have no dependency
// on the vendor realtime adapter.
func remoteLiveTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "live remote test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	certificate := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"runtime.test"},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, public, caPrivate)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	}
	server := &tls.Config{Certificates: []tls.Certificate{certificate(2, x509.ExtKeyUsageServerAuth)}, ClientCAs: roots}
	client := &tls.Config{Certificates: []tls.Certificate{certificate(3, x509.ExtKeyUsageClientAuth)}, RootCAs: roots, ServerName: "runtime.test"}
	return server, client
}

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
func (imageRemoteInventory) Offer(routerwire.OfferRequest) (routerwire.OfferSnapshot, error) {
	return routerwire.OfferSnapshot{}, nil
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

// recordingRemoteImage keeps the request a remote runtime receives.
type recordingRemoteImage struct{ started chan wire.ImageRequest }

func (r *recordingRemoteImage) Start(req wire.ImageRequest) (wire.Admission, error) {
	r.started <- req
	return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "fixture"}, nil
}
func (*recordingRemoteImage) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (*recordingRemoteImage) Cancel(string) (wire.Cancellation, error) {
	return wire.Cancellation{}, nil
}

// [INF-I1] The remote relay forwards the contract mask by digest: the remote
// runtime's Start carries the same mask digest and media type, and this
// runtime reads neither input; the receiving runtime checks and converts.
func TestRemoteRelayForwardsTheContractMaskByDigest(t *testing.T) {
	serverTLS, clientTLS := remoteLiveTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteImage := &recordingRemoteImage{started: make(chan wire.ImageRequest, 1)}
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
	provider, err := inference.New(inference.Config{Router: routes, SurveyAge: time.Hour,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveContent: func(context.Context, inference.Subject, string, int64) ([]byte, inference.ContentOutcome) {
			t.Error("the remote relay read an input locally")
			return nil, inference.ContentUnknown
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return nil, inference.ContentUnavailable
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	image, mask := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	req := wire.ImageRequest{Model: "gpt-image-1", Mode: wire.ImageModeEdit, Prompt: "snow", Size: "1024x1024", Count: 1,
		ImageDigest: image, ImageMediaType: "image/png", MaskDigest: mask, MaskMediaType: "image/png",
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "remote-key", Extensions: map[string]string{}}
	got := provider.StartImage(context.Background(), inference.Subject{Account: "account", Program: "/apps/images"}, req)
	if got.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", got)
	}
	select {
	case forwarded := <-remoteImage.started:
		if forwarded.ImageDigest != image || forwarded.MaskDigest != mask || forwarded.MaskMediaType != "image/png" || forwarded.ImageMediaType != "image/png" {
			t.Fatalf("forwarded %+v", forwarded)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the remote runtime received no Start")
	}
}
