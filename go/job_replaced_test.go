package inference

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// nativeVideoHost is the router host a declaration of the video provider at
// endpoint makes, as newVideoRuntime builds it.
func nativeVideoHost(t *testing.T, endpoint string) *router.Host {
	t.Helper()
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
	host, err := router.NewNative("video-provider", endpoint, listen.ServerExpectation{Principal: principal, Program: identity.CanonicalProgramPath(filepath.Clean(exe))},
		[]string{testVideoModel}, []string{router.ProfileVideo})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// awaitWaiting waits until the job's snapshot shows waiting word want.
func (v *videoRuntime) awaitWaiting(t *testing.T, id acceptance.RequestIdentity, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		observed, err := v.jobs.BindOperations("caller-scope").ObserveWork(id)
		if err == nil && observed.Snapshot != nil && observed.Snapshot.Waiting == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	observed, _ := v.jobs.BindOperations("caller-scope").ObserveWork(id)
	t.Fatalf("waiting %q never shown: %+v", want, observed.Snapshot)
}

// INF-G10: a job whose recorded server is withdrawn waits for it as
// host:unavailable for a bounded time, is then planned afresh once, and fails
// with the router's own reason when no server serves its model any more.
func TestAJobWhoseServerIsGoneWaitsABoundedTimeThenFails(t *testing.T) {
	saved := recordedHostWait
	recordedHostWait = time.Second
	t.Cleanup(func() { recordedHostWait = saved })
	gone := &fakeVideoProvider{loseStart: 1 << 20}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, gone), nil)
	submission, accepted := v.submit(t, "video-gone", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	v.awaitWaiting(t, submission.Identity, "upstream:uncertain")
	v.execution.provider.cfg.Router.SetHosts()
	v.awaitWaiting(t, submission.Identity, "host:unavailable")
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil || !strings.HasPrefix(final.Failure.Message, "host:router:") {
		t.Fatalf("a job whose server is gone: %+v failure %+v", final, final.Failure)
	}
}

// INF-G10: the wait for a gone server counts from the start of the outage
// at hand. A server that was gone briefly and came back leaves no mark, so
// a second short outage beginning after the wait's length since the first
// waits again as host:unavailable instead of being planned afresh at once.
// The review of fa380274e found the first outage's time kept.
func TestASecondShortOutageWaitsAgain(t *testing.T) {
	saved := recordedHostWait
	recordedHostWait = 2 * time.Second
	t.Cleanup(func() { recordedHostWait = saved })
	gone := &fakeVideoProvider{loseStart: 1 << 20}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, gone), nil)
	hosts := v.execution.provider.cfg.Router.Hosts()
	submission, accepted := v.submit(t, "video-flap", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	v.awaitWaiting(t, submission.Identity, "upstream:uncertain")
	// The first outage, shorter than the wait.
	v.execution.provider.cfg.Router.SetHosts()
	v.awaitWaiting(t, submission.Identity, "host:unavailable")
	v.execution.provider.cfg.Router.SetHosts(hosts...)
	v.awaitWaiting(t, submission.Identity, "upstream:uncertain")
	// Longer than the wait after the first outage began, a second one.
	time.Sleep(3 * time.Second)
	v.execution.provider.cfg.Router.SetHosts()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		observed, err := v.jobs.BindOperations("caller-scope").ObserveWork(submission.Identity)
		if err == nil && observed.Snapshot != nil {
			switch observed.Snapshot.Waiting {
			case "host:unavailable":
				return
			case "host:gone:replanned":
				t.Fatalf("a fresh outage was treated as %s at once: the wait counted from the first outage", observed.Snapshot.Waiting)
			}
			if observed.Snapshot.Failure != nil {
				t.Fatalf("a fresh outage failed the job: %+v", observed.Snapshot.Failure)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no waiting word seen")
}

// INF-G10: a local job recorded on a server whose declaration was then
// replaced is planned afresh once, onto the server the router picks now; a
// second replacement fails it as host:replaced. On 2026-10-02 a video job
// accepted before its provider's declaration was replaced waited
// host:replaced until it was cancelled (research/live-runs/2026-10-02.md,
// phase 2 finding 2).
func TestALocalJobOnAReplacedServerIsPlannedAfreshOnceThenFails(t *testing.T) {
	// The first provider answers no Start, so the job stays recorded on it
	// as submitting, as the live job was.
	first := &fakeVideoProvider{loseStart: 1 << 20}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, first), nil)
	submission, accepted := v.submit(t, "video-replaced", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	v.awaitWaiting(t, submission.Identity, "upstream:uncertain")

	// The declaration is replaced: the same name now names another provider,
	// which completes the clip.
	second := &fakeVideoProvider{script: []wire.VideoObservation{{Status: wire.VideoStateGenerating, Step: 1, Steps: 3}}}
	v.execution.provider.cfg.Router.SetHosts(nativeVideoHost(t, serveFakeVideoProvider(t, second)))
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("the replanned job did not complete: %+v failure %+v", final, final.Failure)
	}
	if starts, _ := second.counts(); starts != 1 {
		t.Fatalf("the replacement saw %d starts", starts)
	}
	second.mu.Lock()
	sameRequest := second.starts[0].RequestID == final.Receipt.OperationID
	second.mu.Unlock()
	if !sameRequest {
		t.Fatal("the replanned job did not keep its request_id")
	}

	// Planned afresh once, a job whose server is replaced again fails.
	third := &fakeVideoProvider{loseStart: 1 << 20}
	fourth := &fakeVideoProvider{script: []wire.VideoObservation{{Status: wire.VideoStateGenerating, Step: 1, Steps: 3}}}
	v.execution.provider.cfg.Router.SetHosts(nativeVideoHost(t, serveFakeVideoProvider(t, third)))
	again, accepted := v.submit(t, "video-replaced-twice", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.awaitWaiting(t, again.Identity, "upstream:uncertain")
	fifth := &fakeVideoProvider{loseStart: 1 << 20}
	v.execution.provider.cfg.Router.SetHosts(nativeVideoHost(t, serveFakeVideoProvider(t, fifth)))
	deadline := time.Now().Add(30 * time.Second)
	for starts, _ := fifth.counts(); starts == 0; starts, _ = fifth.counts() {
		if time.Now().After(deadline) {
			t.Fatal("the job was not planned afresh onto the first replacement")
		}
		time.Sleep(10 * time.Millisecond)
	}
	v.execution.provider.cfg.Router.SetHosts(nativeVideoHost(t, serveFakeVideoProvider(t, fourth)))
	failed := v.waitVideo(t, again.Identity)
	if failed.State != acceptance.WorkStateFailed || failed.Failure == nil || failed.Failure.Message != "host:replaced" {
		t.Fatalf("a job replaced twice: %+v failure %+v", failed, failed.Failure)
	}
	if starts, _ := fourth.counts(); starts != 0 {
		t.Fatalf("a job replaced twice was sent again (%d starts)", starts)
	}
}
