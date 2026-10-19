package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	casapi "github.com/openabstractions/abstraction-cas/go/api"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

var nativeImagePNG = []byte("\x89PNG\r\n\x1a\nnative-image")

type nativeTestEngine struct {
	waitCancel       bool
	expectExtensions bool
	started          chan struct{}
	cancelled        chan struct{}
}

func (e nativeTestEngine) Unsupported(req wire.ImageRequest) string {
	if e.expectExtensions && (req.Extensions["fixture/steps"] != "4" || len(req.RequiredExtensions) != 1 || req.RequiredExtensions[0] != "fixture/steps") {
		return "extension:fixture/steps"
	}
	return ""
}
func (e nativeTestEngine) Run(ctx context.Context, _ string, _ wire.ImageRequest, _ map[string]inference.ImageInput, progress func(int64, string)) inference.ImageEngineReply {
	close(e.started)
	if e.waitCancel {
		progress(1, "started")
		<-ctx.Done()
		close(e.cancelled)
		return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "caller"}
	}
	return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []inference.ImageOutput{{Data: nativeImagePNG, MediaType: "image/png"}}}
}

func TestNativeImageCopiesProviderOutputIntoCallerStore(t *testing.T) {
	runNativeImageScenario(t, false, true, false, false)
}

func TestNativeImageEditUploadsVerifiedInput(t *testing.T) {
	runNativeImageScenario(t, true, true, false, false)
}

func TestNativeImageDoesNotForwardInaccessibleOutputDigest(t *testing.T) {
	runNativeImageScenario(t, false, false, false, false)
}

func TestNativeImageCancelPropagatesToProviderEngine(t *testing.T) {
	runNativeImageScenario(t, false, true, true, false)
}

func TestNativeImageDeniedRequestNeverStartsProvider(t *testing.T) {
	runNativeImageScenario(t, false, true, false, true)
}

func TestNativeImageAdvertisedRequiredExtensionReachesSelectedProvider(t *testing.T) {
	runNativeImageScenario(t, false, true, false, false, true)
}

func TestNativeImageUnadvertisedRequiredExtensionRefusesBeforeAdmission(t *testing.T) {
	runNativeImageScenario(t, false, true, false, false, true, false)
}

func runNativeImageScenario(t *testing.T, edit, persistOutput, cancelWork, deny bool, require ...bool) {
	withExtension := len(require) > 0 && require[0]
	advertiseExtension := len(require) < 2 || require[1]
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("engine", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context, *identity.Peer, string) error { return nil }
	content, err := storageService.NewComponent(store, allow)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.EnableWriter(allow, 20<<20, casapi.BoundedFileStore{MaxBytes: storageService.MaxRecordFileBytes}, filepath.Join(t.TempDir(), "uploads.json")); err != nil {
		t.Fatal(err)
	}
	engineHost, err := router.NewImageEngine("engine", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	engine := nativeTestEngine{waitCancel: cancelWork, expectExtensions: withExtension, started: make(chan struct{}), cancelled: make(chan struct{})}
	leaf, err := inference.New(inference.Config{Router: router.New(engineHost), ImageEngine: engine, ImageEngineHost: engineHost,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveContent: func(_ context.Context, _ inference.Subject, digest string, max int64) ([]byte, inference.ContentOutcome) {
			ref, ok := store.Find(digest)
			if !ok || ref.Size > max {
				return nil, inference.ContentUnavailable
			}
			data, err := os.ReadFile(store.Path(ref))
			if err != nil {
				return nil, inference.ContentUnavailable
			}
			return data, inference.ContentResolved
		},
		PrepareContentWrite: func(_ context.Context, _ inference.Subject, _, _ string, _ int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
				if !persistOutput {
					return inference.ContentResolved
				}
				ref, err := store.Place(digest, int64(len(data)))
				if err != nil {
					return inference.ContentUnavailable
				}
				if err := os.WriteFile(store.Path(ref), data, 0600); err != nil {
					return inference.ContentUnavailable
				}
				if err := store.Commit(ref); err != nil {
					return inference.ContentUnavailable
				}
				return inference.ContentResolved
			}, inference.ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	h, err := ListenLeaf(endpoint, leaf, inference.ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	h.Content = content
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	defer func() { stop(); <-done; leaf.Close() }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("native", endpoint, listen.ServerExpectation{Principal: principal, Program: identity.CanonicalProgramPath(filepath.Clean(exe))}, []string{"stability/sdxl"}, []string{router.ProfileImage})
	if err != nil {
		t.Fatal(err)
	}
	if withExtension && advertiseExtension {
		native.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: {"fixture/steps": true}}
	}
	var saved []byte
	var admits atomic.Int64
	mediator, err := inference.New(inference.Config{Router: router.New(native),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			if deny {
				return "denied", nil
			}
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		Admit: func(context.Context, *router.Host, string) (string, bool) { admits.Add(1); return "", true },
		ResolveContent: func(_ context.Context, _ inference.Subject, digest string, _ int64) ([]byte, inference.ContentOutcome) {
			sum := sha256.Sum256(nativeImagePNG)
			if digest != "sha256:"+hex.EncodeToString(sum[:]) {
				return nil, inference.ContentForbidden
			}
			return bytes.Clone(nativeImagePNG), inference.ContentResolved
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(_ context.Context, _ string, data []byte) inference.ContentOutcome {
				saved = append([]byte(nil), data...)
				return inference.ContentResolved
			}, inference.ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mediator.Close()
	subject := inference.Subject{Account: "test", Program: "test"}
	req := wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
	if withExtension {
		req.Extensions = map[string]string{"fixture/steps": "4"}
		req.RequiredExtensions = []string{"fixture/steps"}
	}
	if edit {
		sum := sha256.Sum256(nativeImagePNG)
		req.Mode, req.ImageDigest, req.ImageMediaType = wire.ImageModeEdit, "sha256:"+hex.EncodeToString(sum[:]), "image/png"
	}
	a := mediator.StartImage(context.Background(), subject, req)
	if withExtension && !advertiseExtension {
		if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "extension:fixture/steps" || admits.Load() != 0 {
			t.Fatalf("unsupported admission %+v, admits %d", a, admits.Load())
		}
		select {
		case <-engine.started:
			t.Fatal("unsupported required key started provider engine")
		default:
		}
		return
	}
	if deny {
		if a.Outcome != wire.StartOutcomeNotPermitted || admits.Load() != 0 {
			t.Fatalf("denied admission %+v, admits %d", a, admits.Load())
		}
		select {
		case <-engine.started:
			t.Fatal("denied request started provider engine")
		default:
		}
		return
	}
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if cancelWork {
		select {
		case <-engine.started:
		case <-time.After(5 * time.Second):
			t.Fatal("provider engine did not start")
		}
		progress := mediator.ObserveImage(context.Background(), subject, a.Operation, 0, 256, 65536, 5000)
		if progress.Outcome != wire.PageOutcomePage || len(progress.Deltas) == 0 || progress.Deltas[0].ImageProgress == nil {
			t.Fatalf("provider progress not relayed before cancellation %+v", progress)
		}
		if result := mediator.CancelImage(subject, a.Operation); result.Outcome != wire.CancelOutcomeCancelled {
			t.Fatalf("cancel %+v", result)
		}
		select {
		case <-engine.cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("provider engine did not receive cancellation")
		}
	}
	var end *wire.ImageReply
	for cursor := int64(0); end == nil; {
		page := mediator.ObserveImage(context.Background(), subject, a.Operation, cursor, 256, 65536, 20000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("image page %+v", page)
		}
		for _, delta := range page.Deltas {
			if delta.ImageEnd != nil {
				end = delta.ImageEnd
			}
		}
		if page.AtEnd && end == nil {
			t.Fatalf("missing image terminal %+v", page)
		}
		cursor = page.Next
	}
	if cancelWork {
		if end.Outcome != wire.ReplyOutcomeCancelled {
			t.Fatalf("cancelled native image end %+v", end)
		}
		return
	}
	if !persistOutput {
		if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "native:content" || saved != nil {
			t.Fatalf("inaccessible provider output %+v, saved %d", end, len(saved))
		}
		return
	}
	if end == nil || end.Outcome != wire.ReplyOutcomeCompleted || len(end.Deliveries) != 1 || end.Deliveries[0].Location != "local" || !bytes.Equal(saved, nativeImagePNG) {
		t.Fatalf("native image end %+v, saved %d", end, len(saved))
	}
	sum := sha256.Sum256(saved)
	if end.Deliveries[0].Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("delivery digest %+v", end.Deliveries[0])
	}
}
