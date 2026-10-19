package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func imageV2Request(target wire.Target) wire.ImageRequestV2 {
	return wire.ImageRequestV2{Target: target, Content: wire.ImageRequestContent{Mode: wire.ImageModeGenerate,
		Prompt: "a lighthouse", Size: "1024x1024", Count: 1, Guarantees: []wire.RequestGuarantee{}, Extensions: map[string]string{}}}
}

func TestImageV2ExplicitAndPinnedPolicyUseImageAdmission(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	f.provider.cfg.Router.Hosts()[0].BindingID = "declaration-1"
	f.provider.cfg.ResolveSelectionV2 = func(_ context.Context, subject Subject, name string) (wire.SelectionPolicyV2, bool, error) {
		if subject != caller || name != "images" {
			t.Fatalf("unbound policy lookup: %+v %q", subject, name)
		}
		return wire.SelectionPolicyV2{Name: name, Candidates: []wire.SelectionCandidateV2{{Model: "gpt-image-1", Placement: wire.CandidatePlacementHosted, Credential: "openai", Provider: "openai"}}}, true, nil
	}
	explicit := imageV2Request(wire.Target{Model: "gpt-image-1"})
	explicit.Content.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}
	explicit.Content.Credential = "openai"
	a := f.provider.StartImageV2(context.Background(), caller, explicit)
	if a.Outcome != wire.StartOutcomeAccepted || a.SelectionReason == nil || *a.SelectionReason != wire.SelectionReasonExplicitModel {
		t.Fatalf("explicit: %+v", a)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("explicit end: %+v", end)
	}
	a = f.provider.StartImageV2(context.Background(), caller, imageV2Request(wire.Target{SelectionPolicy: "images"}))
	if a.Outcome != wire.StartOutcomeAccepted || a.Host != "openai" || a.PreferredModel != "gpt-image-1" || a.SelectionReason == nil || *a.SelectionReason != wire.SelectionReasonPreferred {
		t.Fatalf("policy: %+v", a)
	}
	_, end = drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("policy end: %+v", end)
	}
}

func TestImageV2MissingPinAndInlineOverrideRefuseBeforeSpend(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { t.Fatalf("unexpected paid request %s", r.URL) })
	f.provider.cfg.ResolveSelectionV2 = func(context.Context, Subject, string) (wire.SelectionPolicyV2, bool, error) {
		return wire.SelectionPolicyV2{Name: "images", Candidates: []wire.SelectionCandidateV2{
			{Model: "gpt-image-1", Placement: wire.CandidatePlacementHosted, Credential: "openai", Provider: "removed-provider"},
			{Model: "gpt-image-1", Placement: wire.CandidatePlacementHosted, Credential: "openai"},
		}}, true, nil
	}
	req := imageV2Request(wire.Target{SelectionPolicy: "images"})
	a := f.provider.StartImageV2(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeNoHost || a.Reason != "selection:provider_stale" || a.SelectionReason != nil {
		t.Fatalf("missing pin: %+v", a)
	}
	req.Content.Credential = "openai"
	a = f.provider.StartImageV2(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "selection:override" {
		t.Fatalf("inline override: %+v", a)
	}
}
