package service

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// recordingVideo is a VideoServer that keeps the caller of every call.
type recordingVideo struct {
	mu      sync.Mutex
	callers []inference.Subject
}

func (v *recordingVideo) seen(caller inference.Subject) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.callers = append(v.callers, caller)
}

func (v *recordingVideo) Start(_ context.Context, caller inference.Subject, request wire.VideoRequest) wire.VideoAdmission {
	v.seen(caller)
	return wire.VideoAdmission{Outcome: wire.StartOutcomeAccepted, Operation: "op-" + request.RequestID, RetentionMs: 1000}
}

func (v *recordingVideo) Observe(_ context.Context, caller inference.Subject, operation string) wire.VideoObservation {
	v.seen(caller)
	return wire.VideoObservation{Status: wire.VideoStateQueued, QueuePosition: 1}
}

func (v *recordingVideo) Cancel(_ context.Context, caller inference.Subject, operation string) wire.Cancellation {
	v.seen(caller)
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

// serveVideoLeaf serves a designated leaf with video on an endpoint; the
// designated program is designated, and the client is this test binary. An
// empty designated serves the endpoint with Listen, which designates none.
func serveVideoLeaf(t *testing.T, designated string, video VideoServer) wire.VideoTransport {
	t.Helper()
	if designated == "" {
		return serveVideoDesignating(t, nil, video)
	}
	return serveVideoDesignating(t, []string{designated}, video)
}

// heldVideo admits a request_id once per caller program, as a provider's
// operation table does.
type heldVideo struct {
	mu       sync.Mutex
	held     map[string]string
	admitted int
	observed string
	canceled string
}

func (v *heldVideo) Start(_ context.Context, caller inference.Subject, request wire.VideoRequest) wire.VideoAdmission {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := caller.Program + "\x00" + request.RequestID
	if operation, ok := v.held[key]; ok {
		return wire.VideoAdmission{Outcome: wire.StartOutcomeAccepted, Operation: operation, RetentionMs: 1000}
	}
	v.admitted++
	v.held[key] = "op-" + request.RequestID
	return wire.VideoAdmission{Outcome: wire.StartOutcomeAccepted, Operation: v.held[key], RetentionMs: 1000}
}

func (v *heldVideo) Observe(_ context.Context, caller inference.Subject, _ string) wire.VideoObservation {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.observed = caller.Program
	return wire.VideoObservation{Status: wire.VideoStateQueued, QueuePosition: 1}
}

func (v *heldVideo) Cancel(_ context.Context, caller inference.Subject, _ string) wire.Cancellation {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.canceled = caller.Program
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

// I-12: a job one designated runtime executable started and another
// reclaims is admitted once. The provider holds request_id under the
// runtime's designation, the same for every executable the leaf names.
func TestVideoReclaimedByAnotherRuntimeExecutableIsAdmittedOnce(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(t.TempDir(), "openabstractions.exe")
	if err := os.WriteFile(sibling, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	video := &heldVideo{held: map[string]string{}}
	// The sibling runtime executable started the job before this one
	// reclaimed it.
	video.Start(context.Background(), inference.Subject{Account: "a", Program: identity.CanonicalProgramPath(sibling)}, wire.VideoRequest{RequestID: "r1"})
	client := wire.NewVideoClient(serveVideoDesignating(t, []string{sibling, exe}, video))
	admission, err := client.Start(wire.VideoRequest{RequestID: "r1", Model: "m", Prompt: "p", Size: "auto", DurationMs: 1000})
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted || admission.Operation != "op-r1" {
		t.Fatalf("start %+v %v", admission, err)
	}
	// The reclaiming executable observes and cancels the sibling's
	// operation as the same designation.
	if observed, err := client.Observe("op-r1"); err != nil || observed.Status != wire.VideoStateQueued {
		t.Fatalf("observe %+v %v", observed, err)
	}
	if cancelled, err := client.Cancel("op-r1"); err != nil || cancelled.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	video.mu.Lock()
	defer video.mu.Unlock()
	if video.admitted != 1 {
		t.Fatalf("the reclaimed job was admitted %d times", video.admitted)
	}
	designation := identity.CanonicalProgramPath(sibling)
	if video.observed != designation || video.canceled != designation {
		t.Fatalf("observe as %q, cancel as %q, want the designation %q", video.observed, video.canceled, designation)
	}
}

// serveVideoDesignating serves a leaf designating every program in
// designated, in order; none serves the endpoint with Listen.
func serveVideoDesignating(t *testing.T, designated []string, video VideoServer) wire.VideoTransport {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	leaf, err := inference.New(inference.Config{Router: router.New(),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	var h *Host
	if len(designated) == 0 {
		h, err = Listen(endpoint, leaf)
	} else {
		h, err = ListenDesignatedLeaf(endpoint, leaf, inference.ExecutionLocal, designated...)
	}
	if err != nil {
		leaf.Close()
		t.Fatal(err)
	}
	h.Video = video
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { stop(); <-done; leaf.Close() })
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
	return listen.FrameClient{Endpoint: endpoint, Server: &listen.ServerExpectation{Principal: principal,
		Program: identity.CanonicalProgramPath(filepath.Clean(exe))}, Timeout: 10 * time.Second, MaxFrame: 1 << 20}
}

// INF-G5: video@1 is served on the provider's endpoint to its designated
// runtime, with the bound caller.
func TestVideoServedToTheDesignatedRuntime(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	video := &recordingVideo{}
	client := wire.NewVideoClient(serveVideoLeaf(t, exe, video))
	admission, err := client.Start(wire.VideoRequest{RequestID: "r1", Model: "m", Prompt: "p", Size: "auto", DurationMs: 1000})
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted || admission.Operation != "op-r1" {
		t.Fatalf("start %+v %v", admission, err)
	}
	if observed, err := client.Observe("op-r1"); err != nil || observed.Status != wire.VideoStateQueued {
		t.Fatalf("observe %+v %v", observed, err)
	}
	if cancelled, err := client.Cancel("op-r1"); err != nil || cancelled.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	video.mu.Lock()
	defer video.mu.Unlock()
	if len(video.callers) != 3 {
		t.Fatalf("callers %+v", video.callers)
	}
	for _, caller := range video.callers {
		if !identity.SameSubjectProgram(caller.Program, identity.CanonicalProgramPath(filepath.Clean(exe))) || caller.Account == "" {
			t.Fatalf("caller %+v", caller)
		}
	}
}

// A caller other than the designated runtime is forbidden before the
// provider's video server sees the call.
func TestVideoRefusesAnUndesignatedCaller(t *testing.T) {
	other := filepath.Join(t.TempDir(), "openabstractions.exe")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	video := &recordingVideo{}
	client := wire.NewVideoClient(serveVideoLeaf(t, other, video))
	if admission, err := client.Start(wire.VideoRequest{RequestID: "r1", Model: "m", Prompt: "p", Size: "auto", DurationMs: 1000}); err != nil || admission.Outcome != wire.StartOutcomeForbidden {
		t.Fatalf("start %+v %v", admission, err)
	}
	if observed, err := client.Observe("op-r1"); err != nil || observed.Status != wire.VideoStateForbidden {
		t.Fatalf("observe %+v %v", observed, err)
	}
	if cancelled, err := client.Cancel("op-r1"); err != nil || cancelled.Outcome != wire.CancelOutcomeForbidden {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	if len(video.callers) != 0 {
		t.Fatalf("the provider saw %+v", video.callers)
	}
}

// An endpoint that designates no runtime, served with Listen, answers every
// video call forbidden: the same account's own programs bind there, and
// video@1 is for a designated runtime alone.
func TestVideoRefusesEveryCallerWhereNoRuntimeIsDesignated(t *testing.T) {
	video := &recordingVideo{}
	client := wire.NewVideoClient(serveVideoLeaf(t, "", video))
	if admission, err := client.Start(wire.VideoRequest{RequestID: "r1", Model: "m", Prompt: "p", Size: "auto", DurationMs: 1000}); err != nil || admission.Outcome != wire.StartOutcomeForbidden {
		t.Fatalf("start %+v %v", admission, err)
	}
	if observed, err := client.Observe("op-r1"); err != nil || observed.Status != wire.VideoStateForbidden {
		t.Fatalf("observe %+v %v", observed, err)
	}
	if cancelled, err := client.Cancel("op-r1"); err != nil || cancelled.Outcome != wire.CancelOutcomeForbidden {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	if len(video.callers) != 0 {
		t.Fatalf("the provider saw %+v", video.callers)
	}
}

// An endpoint without a video server refuses the profile.
func TestVideoProfileAbsentWithoutAServer(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	client := wire.NewVideoClient(serveVideoLeaf(t, exe, nil))
	if _, err := client.Start(wire.VideoRequest{RequestID: "r1", Model: "m", Prompt: "p", Size: "auto", DurationMs: 1000}); err == nil {
		t.Fatal("video@1 answered without a server")
	}
}
