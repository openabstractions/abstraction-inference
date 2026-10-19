package service

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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

// strictEngine refuses every extension key it does not own, as TheNoise's
// engine adapter does, and keeps each request it was asked to check or run.
type strictEngine struct {
	mu      sync.Mutex
	checked []wire.ImageRequest
	ran     []wire.ImageRequest
}

var strictEngineKeys = map[string]bool{"fixture/steps": true, "fixture/seed": true}

func (e *strictEngine) Unsupported(req wire.ImageRequest) string {
	e.mu.Lock()
	e.checked = append(e.checked, req)
	e.mu.Unlock()
	for _, key := range req.RequiredExtensions {
		if _, present := req.Extensions[key]; !strictEngineKeys[key] || !present {
			return "extension:" + key
		}
	}
	keys := make([]string, 0, len(req.Extensions))
	for key := range req.Extensions {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !strictEngineKeys[key] {
			return "extension:" + key
		}
	}
	return ""
}

func (e *strictEngine) Run(_ context.Context, _ string, req wire.ImageRequest, _ map[string]inference.ImageInput, _ func(int64, string)) inference.ImageEngineReply {
	e.mu.Lock()
	e.ran = append(e.ran, req)
	e.mu.Unlock()
	return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []inference.ImageOutput{{Data: nativeImagePNG, MediaType: "image/png"}}}
}

func (e *strictEngine) seen() (checked, ran []wire.ImageRequest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.checked), slices.Clone(e.ran)
}

type recordLog struct {
	mu      sync.Mutex
	records []inference.Record
}

func (l *recordLog) add(r inference.Record) {
	l.mu.Lock()
	l.records = append(l.records, r)
	l.mu.Unlock()
}

// wait returns the first record matching want, polling briefly because the
// provider records an operation after emitting its terminal delta.
func (l *recordLog) wait(t *testing.T, want func(inference.Record) bool) inference.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l.mu.Lock()
		for _, r := range l.records {
			if want(r) {
				l.mu.Unlock()
				return r
			}
		}
		all := slices.Clone(l.records)
		l.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no matching record in %+v", all)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasReservedKey(req wire.ImageRequest) bool {
	for key := range req.Extensions {
		if strings.HasPrefix(key, reservedPrefix) {
			return true
		}
	}
	for _, key := range req.RequiredExtensions {
		if strings.HasPrefix(key, reservedPrefix) {
			return true
		}
	}
	return false
}

type claimLeaf struct {
	endpoint string
	engine   *strictEngine
	records  *recordLog
	program  string
}

// startClaimLeaf serves a strict image engine on a leaf endpoint. designated
// names this test executable as the leaf's designated runtime.
func startClaimLeaf(t *testing.T, designated bool) claimLeaf {
	t.Helper()
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
	engine, records := &strictEngine{}, &recordLog{}
	leaf, err := inference.New(inference.Config{Router: router.New(engineHost), ImageEngine: engine, ImageEngineHost: engineHost, Record: records.add,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
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
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(exe))
	endpoint := endpointFor(t)
	var h *Host
	if designated {
		h, err = ListenDesignatedLeaf(endpoint, leaf, inference.ExecutionLocal, exe)
	} else {
		h, err = ListenLeaf(endpoint, leaf, inference.ExecutionLocal)
	}
	if err != nil {
		leaf.Close()
		t.Fatal(err)
	}
	h.Content = content
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { stop(); <-done; leaf.Close() })
	return claimLeaf{endpoint: endpoint, engine: engine, records: records, program: program}
}

// startClaimMediator is the OA runtime side: a provider whose selected host is
// the native leaf, advertising the given image extension keys.
func startClaimMediator(t *testing.T, leaf claimLeaf, advertised ...string) (*inference.Provider, *recordLog) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("native", leaf.endpoint, listen.ServerExpectation{Principal: principal, Program: leaf.program}, []string{"stability/sdxl"}, []string{router.ProfileImage})
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, key := range advertised {
		known[key] = true
	}
	native.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: known}
	records := &recordLog{}
	mediator, err := inference.New(inference.Config{Router: router.New(native), Record: records.add,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(context.Context, string, []byte) inference.ContentOutcome { return inference.ContentResolved }, inference.ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mediator.Close() })
	return mediator, records
}

func observeImageEnd(t *testing.T, observe func(cursor int64) wire.DeltaPage) wire.ImageReply {
	t.Helper()
	for cursor := int64(0); ; {
		page := observe(cursor)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("image page %+v", page)
		}
		for _, delta := range page.Deltas {
			if delta.ImageEnd != nil {
				return *delta.ImageEnd
			}
		}
		if page.AtEnd {
			t.Fatalf("missing image terminal %+v", page)
		}
		cursor = page.Next
	}
}

// reservedPrefix is the OA-owned extension namespace, spelled here so the
// regressions compile against the unfixed provider.
const reservedPrefix = "openabstractions/"

var claimApp = inference.Subject{Account: "test", Program: "app-program"}

func claimRequest(extensions map[string]string, required ...string) wire.ImageRequest {
	return wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "512x512", Count: 1,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}, Extensions: extensions, RequiredExtensions: required}
}

// HP3: the runtime's native image forwarder adds the bound caller claim. A
// strict engine must receive the required vendor keys and never that claim.
func TestDesignatedLeafEngineReceivesRequiredVendorKeysWithoutClaim(t *testing.T) {
	leaf := startClaimLeaf(t, true)
	mediator, _ := startClaimMediator(t, leaf, "fixture/steps", "fixture/seed")
	a := mediator.StartImage(context.Background(), claimApp, claimRequest(map[string]string{"fixture/steps": "4", "fixture/seed": "42"}, "fixture/steps", "fixture/seed"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	end := observeImageEnd(t, func(cursor int64) wire.DeltaPage {
		return mediator.ObserveImage(context.Background(), claimApp, a.Operation, cursor, 256, 65536, 20000)
	})
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
	checked, ran := leaf.engine.seen()
	if len(checked) != 1 || len(ran) != 1 {
		t.Fatalf("engine checked %d, ran %d", len(checked), len(ran))
	}
	for _, req := range append(checked, ran...) {
		if hasReservedKey(req) || req.Extensions["fixture/steps"] != "4" || req.Extensions["fixture/seed"] != "42" || len(req.Extensions) != 2 ||
			!slices.Equal(req.RequiredExtensions, []string{"fixture/steps", "fixture/seed"}) {
			t.Fatalf("engine request extensions %v required %v", req.Extensions, req.RequiredExtensions)
		}
	}
}

// The leaf's audit names the designated runtime as Program and the original
// bound caller as Claim, for the accepted operation's terminal record.
func TestDesignatedLeafRecordsOriginalBoundCaller(t *testing.T) {
	leaf := startClaimLeaf(t, true)
	mediator, _ := startClaimMediator(t, leaf, "fixture/steps")
	a := mediator.StartImage(context.Background(), claimApp, claimRequest(map[string]string{"fixture/steps": "4"}, "fixture/steps"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	observeImageEnd(t, func(cursor int64) wire.DeltaPage {
		return mediator.ObserveImage(context.Background(), claimApp, a.Operation, cursor, 256, 65536, 20000)
	})
	record := leaf.records.wait(t, func(r inference.Record) bool { return r.Operation != "" })
	if record.Outcome != "completed" || record.Claim != claimApp.Program || !identity.SameSubjectProgram(record.Program, leaf.program) || record.Route != inference.RouteNative {
		t.Fatalf("leaf record %+v", record)
	}
}

// An application's own claim is replaced by its bound program at the runtime
// and never recorded as the caller by either side.
func TestForgedApplicationClaimIsNotRecordedAsCaller(t *testing.T) {
	leaf := startClaimLeaf(t, true)
	mediator, mediatorRecords := startClaimMediator(t, leaf, "fixture/steps")
	ctx := inference.WithRoute(context.Background(), inference.RouteNative, "program")
	a := mediator.StartImage(ctx, claimApp, claimRequest(map[string]string{"fixture/steps": "4", inference.ClaimExtension: "forged-app"}))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	end := observeImageEnd(t, func(cursor int64) wire.DeltaPage {
		return mediator.ObserveImage(context.Background(), claimApp, a.Operation, cursor, 256, 65536, 20000)
	})
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
	if record := leaf.records.wait(t, func(r inference.Record) bool { return r.Operation != "" }); record.Claim != claimApp.Program {
		t.Fatalf("leaf recorded claim %q", record.Claim)
	}
	if record := mediatorRecords.wait(t, func(r inference.Record) bool { return r.Operation != "" }); record.Claim != "" || record.Program != claimApp.Program {
		t.Fatalf("runtime record %+v", record)
	}
	_, ran := leaf.engine.seen()
	if len(ran) != 1 || hasReservedKey(ran[0]) {
		t.Fatalf("engine runs %+v", ran)
	}
}

// A leaf without a designated runtime trusts no claim: its caller's claim is
// removed before the engine and never recorded.
func TestUndesignatedLeafDiscardsCallerClaim(t *testing.T) {
	leaf := startClaimLeaf(t, false)
	client := wire.NewImageClient(listen.FrameClient{Endpoint: leaf.endpoint, Timeout: 35 * time.Second, MaxFrame: 1 << 20, Sessions: true})
	a, err := client.Start(claimRequest(map[string]string{"fixture/steps": "4", inference.ClaimExtension: "forged-app"}))
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v, %v", a, err)
	}
	end := observeImageEnd(t, func(cursor int64) wire.DeltaPage {
		page, err := client.Observe(a.Operation, cursor, 256, 65536, 20000)
		if err != nil {
			t.Fatal(err)
		}
		return page
	})
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
	if record := leaf.records.wait(t, func(r inference.Record) bool { return r.Operation != "" }); record.Claim != "" {
		t.Fatalf("undesignated leaf recorded claim %q", record.Claim)
	}
	checked, ran := leaf.engine.seen()
	for _, req := range append(checked, ran...) {
		if hasReservedKey(req) {
			t.Fatalf("engine saw reserved key %v", req.Extensions)
		}
	}
}

// A vendor key the runtime admits but the engine does not implement is refused
// at the leaf before launch. The engine checks only vendor keys and never runs.
func TestUnsupportedVendorKeyRefusedBeforeEngineRun(t *testing.T) {
	leaf := startClaimLeaf(t, true)
	mediator, _ := startClaimMediator(t, leaf, "fixture/steps", "fixture/unknown")
	a := mediator.StartImage(context.Background(), claimApp, claimRequest(map[string]string{"fixture/steps": "4", "fixture/unknown": "1"}, "fixture/steps", "fixture/unknown"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("runtime admission %+v", a)
	}
	end := observeImageEnd(t, func(cursor int64) wire.DeltaPage {
		return mediator.ObserveImage(context.Background(), claimApp, a.Operation, cursor, 256, 65536, 20000)
	})
	if end.Outcome != wire.ReplyOutcomeUnsupportedFeature || end.Reason != "native:extension:fixture/unknown" || end.Images != 0 {
		t.Fatalf("end %+v", end)
	}
	checked, ran := leaf.engine.seen()
	if len(checked) != 1 || len(ran) != 0 || hasReservedKey(checked[0]) {
		t.Fatalf("engine checked %+v, ran %d", checked, len(ran))
	}
	refusal := leaf.records.wait(t, func(r inference.Record) bool { return r.Outcome == "unsupported_feature" })
	if refusal.Operation != "" || refusal.Reason != "extension:fixture/unknown" || refusal.Claim != claimApp.Program {
		t.Fatalf("leaf refusal record %+v", refusal)
	}
}
