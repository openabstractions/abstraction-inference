package inference

import (
	"testing"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
)

var _ acceptanceprovider.RightsRefusalReporter = (*JobExecution)(nil)

// abstraction-job JOB-A19 for inference jobs: a job its plan refused for a
// revoked grant is no result of the work. The same request submitted again
// once the grant is back runs once, as a new operation, and its success then
// replays to a further submission with nothing sent upstream again.
func TestARefusedInferenceJobRunsAgainWhenTheSameRequestIsResubmitted(t *testing.T) {
	video := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: video, MediaType: "video/mp4"}}}}
	fixture := newDurableFixture(t, backend)
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "refused-then-granted", jobwire.ProfileVideo)
	first, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || first.Receipt == nil {
		t.Fatalf("submit %+v %v", first, err)
	}
	fixture.mu.Lock()
	fixture.permitted = false
	fixture.mu.Unlock()
	stop := executeUntilCleanup(t, provider)
	defer stop()
	refused := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if refused.State != acceptance.WorkStateFailed || refused.Failure == nil || refused.Failure.Message != "rights:denied" {
		t.Fatalf("refused job %+v", refused)
	}

	fixture.mu.Lock()
	fixture.permitted = true
	fixture.mu.Unlock()
	again, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || again.Outcome != acceptance.AcceptanceOutcomeAccepted || again.Receipt == nil || again.Receipt.OperationID == first.Receipt.OperationID {
		t.Fatalf("resubmission after the grant %+v %v", again, err)
	}
	if done := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true); done.State != acceptance.WorkStateComplete {
		t.Fatalf("the rerun ended %+v", done)
	}
	backend.mu.Lock()
	submits := backend.submits
	backend.mu.Unlock()
	if submits != 1 {
		t.Fatalf("the rerun reached the upstream %d times", submits)
	}
	replay, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || replay.Receipt == nil || replay.Receipt.OperationID != again.Receipt.OperationID {
		t.Fatalf("a completed job was not replayed %+v %v", replay, err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("a replayed job reached the upstream again: %d submits", backend.submits)
	}
}

// Only grant refusals are refusals by rights: a failure of the work itself
// replays.
func TestOnlyGrantRefusalsCountAsRefusedByRights(t *testing.T) {
	e := &JobExecution{}
	for reason, want := range map[string]bool{
		"rights:denied":                         true,
		"rights:not_granted output:forbidden":   true,
		"content:read:forbidden":                true,
		"credential:not_permitted":              true,
		"upstream:refused":                      false,
		"native:forbidden":                      false,
		"native:x:forbidden":                    false,
		"upstream:forbidden":                    false,
		"provider:forbidden":                    false,
		"rights:native:forbidden":               false,
		"caller:forbidden":                      false,
		"rights:not_granted upstream:malformed": false,
		"":                                      false,
	} {
		record := &job.Record{Kind: InferenceJobKind, State: job.StateFailed}
		if err := record.SetCheckpoint(inferenceJobCheckpoint{Failure: reason}); err != nil {
			t.Fatal(err)
		}
		if got := e.RefusedByRights(record); got != want {
			t.Errorf("%q read %v, want %v", reason, got, want)
		}
	}
}
