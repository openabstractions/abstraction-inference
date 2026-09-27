package inference

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/user"
	"runtime"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// A host declared native but with no working transport reads unavailable
// with a typed reason instead of reaching an address.
func TestNativeEmbedMissingTransportReadsUnavailable(t *testing.T) {
	host := &router.Host{Name: "unwired", Wire: router.WireNative}
	e := nativeEmbed(context.Background(), host, "m", embedRequest("a"))
	if e.Outcome != wire.EmbedOutcomeUnavailable || e.Reason != "native:transport" {
		t.Fatalf("missing transport: %+v", e)
	}
}

// principalOfThisProcess is this test binary's own account, in the identity
// vocabulary a native transport's Server expectation checks the peer
// against; client and fake provider run as the same process here.
func principalOfThisProcess(t *testing.T) identity.User {
	t.Helper()
	if runtime.GOOS == "windows" {
		u, err := user.Current()
		if err != nil || u.Uid == "" {
			t.Fatalf("principal: %v", err)
		}
		return identity.User{Kind: "windows", SID: u.Uid}
	}
	return identity.User{Kind: "posix", UID: os.Getuid()}
}

// embedderFunc adapts a plain function to wire.Embedder.
type embedderFunc func(wire.EmbedRequest) (wire.Embeddings, error)

func (f embedderFunc) Embed(req wire.EmbedRequest) (wire.Embeddings, error) { return f(req) }

// nativeEmbedFixture serves embed@1 alone, on the identity-bound native
// transport, from a minimal fake declared provider, exactly as
// inferenceservice.Host.Serve serves it beside chat@1 in production
// (service/host.go): one ReceiveFramed and one Reply per accepted
// connection. It returns the router.Host a runtime builds for the
// declaration (router.NewNative), reaching this fixture.
func nativeEmbedFixture(t *testing.T, name string, embed func(wire.EmbedRequest) (wire.Embeddings, error)) *router.Host {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	principal := principalOfThisProcess(t)
	endpoint := listen.Endpoint(fmt.Sprintf("inf-embed-%s-%d-%d", name, os.Getpid(), time.Now().UnixNano()))
	l, err := listen.ListenFramed(endpoint, listen.Program)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Close() })
	dispatcher := &wire.EmbedderDispatcher{Handler: embedderFunc(embed)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				callCtx, done := context.WithTimeout(ctx, 5*time.Second)
				defer done()
				call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, 1<<20)
				if err != nil {
					return
				}
				defer call.Close()
				if reply, err := dispatcher.ExchangeFrame(call.Frame); err == nil {
					call.Reply(reply)
				}
			}()
		}
	}()
	host, err := router.NewNative(name, endpoint, listen.ServerExpectation{Principal: principal, Program: exe}, []string{"fixture-model"}, []string{router.ProfileEmbed})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// nativeEmbedRequest names the fixture's own allowlisted model, unlike
// embedRequest's hosted fixture model.
func nativeEmbedRequest(inputs ...string) wire.EmbedRequest {
	return wire.EmbedRequest{Model: "fixture-model", Inputs: inputs}
}

// packVector base64-encodes one little-endian float32 vector, the same wire
// shape embedOpenAI and embedRemote produce.
func packVector(values ...float32) string {
	packed := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(packed[4*i:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(packed)
}

// Embed reaches a declared native provider's own embed@1 exactly as
// Complete reaches its chat@1 (nativeChat): the router picks the native
// host, admits it, and nativeEmbed calls the downstream's OA endpoint over
// the identity-bound transport, forwarding the resolved model name.
func TestNativeEmbedReachesTheProvidersOwnEmbedEndpoint(t *testing.T) {
	var gotModel string
	host := nativeEmbedFixture(t, "fixture-native", func(req wire.EmbedRequest) (wire.Embeddings, error) {
		gotModel = req.Model
		out := wire.Embeddings{Outcome: wire.EmbedOutcomeCompleted, Dimensions: 2, Usage: wire.Usage{Input: int64(len(req.Inputs))}}
		for i := range req.Inputs {
			out.Vectors = append(out.Vectors, packVector(float32(i)+0.5, -1.25))
		}
		return out, nil
	})
	p, err := New(Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e := p.Embed(context.Background(), caller, nativeEmbedRequest("alpha", "beta"))
	if e.Outcome != wire.EmbedOutcomeCompleted || e.Dimensions != 2 || len(e.Vectors) != 2 || e.Host != "fixture-native" || e.Usage.Input != 2 {
		t.Fatalf("native embed: %+v", e)
	}
	if gotModel != "fixture-model" {
		t.Fatalf("model forwarded = %q, want fixture-model", gotModel)
	}
	if e.Vectors[0] != packVector(0.5, -1.25) || e.Vectors[1] != packVector(1.5, -1.25) {
		t.Fatalf("vectors %v", e.Vectors)
	}
}

// A refusal from the downstream's own embed@1 reaches the caller as the
// native source, in the reason vocabulary embedRemote already uses for a
// delegated runtime.
func TestNativeEmbedRefusalCarriesTheNativeSource(t *testing.T) {
	host := nativeEmbedFixture(t, "refusing-native", func(wire.EmbedRequest) (wire.Embeddings, error) {
		return wire.Embeddings{Outcome: wire.EmbedOutcomeRefused, Reason: "model:unavailable"}, nil
	})
	p, err := New(Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	e := p.Embed(context.Background(), caller, nativeEmbedRequest("a"))
	if e.Outcome != wire.EmbedOutcomeRefused || e.Reason != "native:model:unavailable" {
		t.Fatalf("native refusal: %+v", e)
	}
}

// A host whose wire this provider does not recognise fails loudly with a
// typed reason, never silently: the wire itself names the refusal.
func TestEmbedUnknownWireFailsWithATypedReason(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	host := f.provider.cfg.Router.Hosts()[0]
	host.Wire, host.Profiles = "mystery-wire@1", []string{router.ProfileEmbed}
	e := f.provider.Embed(context.Background(), caller, embedRequest("a"))
	if e.Outcome != wire.EmbedOutcomeUnsupportedFeature || e.Reason != "wire:mystery-wire@1" {
		t.Fatalf("unknown wire: %+v", e)
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("an unrecognised wire reached the upstream")
	}
}
