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
	if designated == "" {
		h, err = Listen(endpoint, leaf)
	} else {
		h, err = ListenDesignatedLeaf(endpoint, leaf, inference.ExecutionLocal, designated)
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
