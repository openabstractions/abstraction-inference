package inference

import (
	"context"
	"testing"
	"time"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestInferenceJobRecoveryRefusesChangedBindingBeforeUpstreamAccess(t *testing.T) {
	for _, change := range []string{"endpoint", "generation", "wire", "legacy"} {
		for _, phase := range []string{"submitting", "submitted"} {
			t.Run(change+"/"+phase, func(t *testing.T) {
				backend := &fakeDurableBackend{}
				fixture := newDurableFixture(t, backend)
				host := fixture.provider.cfg.Router.Hosts()[0]
				host.BindingID = "registration-1"
				pin := inferenceJobCheckpoint{Phase: phase, Host: host.Name, Binding: inferenceHostBinding(host), Model: "owner/model", Adapter: "replicate"}
				if phase == "submitted" {
					pin.Handle = "original-operation"
				}
				replacement := *host
				want := "host:replaced"
				switch change {
				case "endpoint":
					replacement.Base = "https://replacement.invalid"
				case "generation":
					replacement.BindingID = "registration-2"
				case "wire":
					replacement.Wire = router.WireOpenAICompatible
				case "legacy":
					pin.Binding = ""
					want = "host:unbound"
				}
				fixture.provider.cfg.Router.SetHosts(&replacement)
				document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
					Model: "owner/model", Mode: jobwire.ModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
					Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
				}}}
				prepared, _, err := fixture.execution.PrepareScoped("caller-scope", "recover", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
				if err != nil {
					t.Fatal(err)
				}
				store := job.NewMemoryStore()
				id, err := store.Submit(job.Record{ID: "recover", Kind: InferenceJobKind, State: job.StatePending, Spec: prepared})
				if err != nil {
					t.Fatal(err)
				}
				held, err := store.Claim(id, "successor", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				held, err = store.Update(id, held.Lease.Epoch, func(record *job.Record) error { return record.SetCheckpoint(pin) })
				if err != nil {
					t.Fatal(err)
				}
				if err := fixture.execution.execute(context.Background(), store, held); err != nil {
					t.Fatal(err)
				}
				current, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				var saved inferenceJobCheckpoint
				if err := current.DecodeCheckpoint(&saved); err != nil {
					t.Fatal(err)
				}
				if saved.Waiting != want || saved.Handle != pin.Handle || saved.Binding != pin.Binding {
					t.Fatalf("ownership changed or wrong refusal: %+v", saved)
				}
				if backend.submits+backend.polls+backend.reconciles+backend.cancels != 0 || fixture.consumer != "" {
					t.Fatal("replacement received upstream work or credential application")
				}
			})
		}
	}
}

func TestInferenceJobCancellationPreservesUncertainSubmission(t *testing.T) {
	backend := &fakeDurableBackend{}
	fixture := newDurableFixture(t, backend)
	host := fixture.provider.cfg.Router.Hosts()[0]
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: "owner/model", Mode: jobwire.ModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
	}}}
	prepared, _, err := fixture.execution.PrepareScoped("caller-scope", "uncertain-cancel", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
	if err != nil {
		t.Fatal(err)
	}
	store := job.NewMemoryStore()
	id, err := store.Submit(job.Record{ID: "uncertain-cancel", Kind: InferenceJobKind, State: job.StatePending, Spec: prepared})
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Claim(id, "successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	held, err = store.Update(id, held.Lease.Epoch, func(record *job.Record) error {
		return record.SetCheckpoint(inferenceJobCheckpoint{Phase: "submitting", Host: host.Name, Binding: inferenceHostBinding(host), Model: "owner/model", Adapter: "replicate"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetIntent(id, job.WantCancel, "caller"); err != nil {
		t.Fatal(err)
	}
	held, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.execution.execute(context.Background(), store, held); err != nil {
		t.Fatal(err)
	}
	current, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var saved inferenceJobCheckpoint
	if err := current.DecodeCheckpoint(&saved); err != nil {
		t.Fatal(err)
	}
	if current.State.Terminal() || current.Wants() != job.WantCancel || saved.Waiting != "upstream:uncertain" {
		t.Fatalf("unknown upstream cancellation reported as finished: state=%s checkpoint=%+v", current.State, saved)
	}
	if backend.submits != 0 || backend.reconciles != 1 || backend.cancels != 0 {
		t.Fatalf("unexpected upstream operations: %+v", backend)
	}
}
