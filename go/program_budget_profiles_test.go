package inference

import (
	"context"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"net/http"
	"path/filepath"
	"testing"
)

func spentProgramBudget(t *testing.T, name string) *Ceilings {
	t.Helper()
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetProgramLimit(name, caller, Ceiling{RequestsPerDay: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddForSubject(name, caller, 2, 3, 0); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProgramBudgetAdmissionCoversChargedProfiles(t *testing.T) {
	testProgramBudgetAdmissionProfiles(t, false)
}

func TestUnreportedCostAdmissionCoversChargedProfiles(t *testing.T) {
	testProgramBudgetAdmissionProfiles(t, true)
}

func testProgramBudgetAdmissionProfiles(t *testing.T, unreported bool) {
	for _, profile := range []string{"chat", "embed", "speech", "transcription", "image", "live"} {
		t.Run(profile, func(t *testing.T) {
			name := "openrouter"
			if profile == "image" {
				name = "openai"
			}
			if profile == "live" {
				name = "live"
			}
			c := spentProgramBudget(t, name)
			want := "budget:requests:" + caller.Program
			wantOutcome := "budget_exceeded"
			if unreported {
				if err := c.SetProgramLimit(name, caller, Ceiling{MicrosPerDay: 100}); err != nil {
					t.Fatal(err)
				}
				if err := c.addModalitiesForSubject(name, caller, 0, 0, 0, 0, 0, false); err != nil {
					t.Fatal(err)
				}
				want = "cost:unreported"
				if profile != "embed" {
					wantOutcome = "exhausted"
				}
			}
			var outcome, reason string
			switch profile {
			case "chat", "embed":
				f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.Ceilings = c })
				posts, uses := len(f.up.posts()), f.applier.count()
				if profile == "chat" {
					a := f.provider.Start(context.Background(), caller, hostedRequest(name))
					outcome, reason = a.Outcome.String(), a.Reason
				} else {
					a := f.provider.Embed(context.Background(), caller, embedRequest("fixture"))
					outcome, reason = a.Outcome.String(), a.Reason
				}
				if len(f.up.posts()) != posts || f.applier.count() != uses {
					t.Fatal("spent program sent work or applied credentials")
				}
			case "speech":
				store := &speechStore{}
				f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.Ceilings = c; config.PrepareContentWrite = store.prepare })
				a := f.provider.StartSpeech(context.Background(), caller, speechRequest("fixture"))
				outcome, reason = a.Outcome.String(), a.Reason
			case "transcription":
				f, audio := transcriptionFixture(t, func(config *Config) { config.Ceilings = c })
				a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
				outcome, reason = a.Outcome.String(), a.Reason
			case "image":
				f := newImageFixture(t, func(http.ResponseWriter, *http.Request, []byte) { t.Error("spent program reached upstream") })
				f.provider.cfg.Ceilings = c
				a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
				outcome, reason = a.Outcome.String(), a.Reason
			case "live":
				f := newLiveFixture(t, false)
				f.p.cfg.Ceilings = c
				a := f.p.StartLive(context.Background(), caller, liveRequest())
				outcome, reason = a.Outcome.String(), a.Reason
			}
			if outcome != wantOutcome || reason != want {
				t.Fatal(outcome, reason)
			}
		})
	}
}

func TestProgramBudgetDurableAccountingRetainsOriginalSubjectOnce(t *testing.T) {
	c := spentProgramBudget(t, "key")
	for range 2 {
		if err := c.addDurableForSubject("attempt", "job-1", "key", &caller, 1, 0); err != nil {
			t.Fatal(err)
		}
		if err := c.addDurableForSubject("images", "job-1", "key", &caller, 0, 2); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := OpenCeilings(c.path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err := reopened.ProgramBudgets("key", caller.Account)
	if err != nil || len(entries) != 1 || entries[0].Requests != 2 || entries[0].Images != 2 {
		t.Fatal(entries, err)
	}
}

func TestProgramBudgetSharesChatVersionsAndObservedUsage(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetProgramLimit("openrouter", caller, Ceiling{RequestsPerDay: 1}); err != nil {
		t.Fatal(err)
	}
	f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.Ceilings = c })
	request := hostedRequest("openrouter")
	first := f.provider.Start(t.Context(), caller, request)
	if first.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(first)
	}
	_, reply, _ := drain(t, f.provider, caller, first.Operation)
	if reply.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatal(reply)
	}
	_, entries, err := c.ProgramBudgets("openrouter", caller.Account)
	if err != nil || len(entries) != 1 || entries[0].Requests != 1 || entries[0].Tokens != reply.Usage.Input+reply.Usage.Output {
		t.Fatal(entries, err)
	}
	second := f.provider.StartV2(t.Context(), caller, wire.RequestV2{Target: wire.Target{Model: request.Model}, Content: wire.RequestContent{Messages: request.Messages, Guarantees: request.Guarantees, Credential: request.Credential}})
	if second.Outcome != wire.StartOutcomeBudgetExceeded || second.Reason != "budget:requests:"+caller.Program {
		t.Fatal(second)
	}
}
