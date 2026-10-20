package inference_test

import (
	"context"
	"testing"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// A selection refusal before any route is chosen is recorded for its proven
// caller, so the caller can read why its request failed (logging LOG-P9,
// A-F19). Chat and image selection share the rule.
func TestSelectionRefusalsBeforeARouteAreRecorded(t *testing.T) {
	records := make(chan inference.Record, 8)
	provider, err := inference.New(inference.Config{Router: router.New(), SurveyAge: time.Hour, Record: func(r inference.Record) { records <- r },
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		ResolveSelectionV2: func(context.Context, inference.Subject, string) (wire.SelectionPolicyV2, bool, error) {
			return wire.SelectionPolicyV2{}, false, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	next := func() inference.Record {
		t.Helper()
		select {
		case r := <-records:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("no record")
			return inference.Record{}
		}
	}
	chat := provider.StartV2(context.Background(), selectionCaller, wire.RequestV2{Target: wire.Target{SelectionPolicy: "everyday"},
		Content: wire.RequestContent{Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
			Guarantees: []wire.RequestGuarantee{}}})
	if chat.Outcome != wire.StartOutcomeNoHost || chat.Reason != "selection:unknown" {
		t.Fatalf("chat admission %+v", chat)
	}
	if r := next(); r.Program != selectionCaller.Program || r.Outcome != "no_host" || r.Reason != "selection:unknown" || r.Profile != "" {
		t.Fatalf("chat record %+v", r)
	}
	image := provider.StartImageV2(context.Background(), selectionCaller, wire.ImageRequestV2{Target: wire.Target{SelectionPolicy: "pictures"},
		Content: wire.ImageRequestContent{Mode: wire.ImageModeGenerate, Prompt: "cat", Size: "auto", Count: 1, Guarantees: []wire.RequestGuarantee{}}})
	if image.Outcome != wire.StartOutcomeNoHost || image.Reason != "selection:unknown" {
		t.Fatalf("image admission %+v", image)
	}
	if r := next(); r.Program != selectionCaller.Program || r.Outcome != "no_host" || r.Reason != "selection:unknown" || r.Profile != router.ProfileImage {
		t.Fatalf("image record %+v", r)
	}
	// An unproven caller has no subject to record.
	if out := provider.StartV2(context.Background(), inference.Subject{}, wire.RequestV2{}); out.Reason != "caller" {
		t.Fatalf("unproven caller %+v", out)
	}
	select {
	case r := <-records:
		t.Fatalf("an unproven caller was recorded: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
