package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// recordLog keeps the records a provider writes; set it before any request.
type recordLog struct {
	mu      sync.Mutex
	records []Record
}

func collectRecords(p *Provider) *recordLog {
	log := &recordLog{}
	p.cfg.Record = func(r Record) {
		log.mu.Lock()
		log.records = append(log.records, r)
		log.mu.Unlock()
	}
	return log
}

// wait returns the first n records once they are written.
func (l *recordLog) wait(t *testing.T, n int) []Record {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		l.mu.Lock()
		got := append([]Record(nil), l.records...)
		l.mu.Unlock()
		if len(got) >= n {
			return got[:n]
		}
	}
	t.Fatalf("fewer than %d records", n)
	return nil
}

func imageV2Request(target wire.Target) wire.ImageRequestV2 {
	return wire.ImageRequestV2{Target: target, Content: wire.ImageRequestContent{Mode: wire.ImageModeGenerate,
		Prompt: "a lighthouse", Size: "1024x1024", Count: 1, Guarantees: []wire.RequestGuarantee{}, Extensions: map[string]string{}}}
}

func TestImageV2ExplicitAndPinnedPolicyUseImageAdmission(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	f.provider.cfg.Router.Hosts()[0].BindingID = "declaration-1"
	records := collectRecords(f.provider)
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
	// F20: the image record names the preferred route that ran; the explicit
	// request's record carries no selection.
	var explicitRecord, policyRecord Record
	for _, r := range records.wait(t, 2) {
		if r.Operation == a.Operation {
			policyRecord = r
		} else {
			explicitRecord = r
		}
	}
	if policyRecord.Profile != router.ProfileImage || policyRecord.Host != "openai" || policyRecord.Model != "gpt-image-1" ||
		policyRecord.Preferred != "gpt-image-1" || policyRecord.PreferredHost != "openai" || policyRecord.Selection != "preferred" {
		t.Fatalf("policy record: %+v", policyRecord)
	}
	if explicitRecord.Preferred != "" || explicitRecord.PreferredHost != "" || explicitRecord.Selection != "" {
		t.Fatalf("explicit record: %+v", explicitRecord)
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
	records := collectRecords(f.provider)
	req := imageV2Request(wire.Target{SelectionPolicy: "images"})
	a := f.provider.StartImageV2(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeNoHost || a.Reason != "selection:provider_stale" || a.SelectionReason != nil {
		t.Fatalf("missing pin: %+v", a)
	}
	// The refusal is an image record of the preferred candidate; nothing was selected.
	if r := records.wait(t, 1)[0]; r.Profile != router.ProfileImage || r.Outcome != "no_host" || r.Model != "gpt-image-1" ||
		r.Preferred != "gpt-image-1" || r.Selection != "" {
		t.Fatalf("missing pin record: %+v", r)
	}
	req.Content.Credential = "openai"
	a = f.provider.StartImageV2(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "selection:override" {
		t.Fatalf("inline override: %+v", a)
	}
}

func TestImageV2CallerLocalOnlyAcceptsLocalPolicy(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(pngOne)}}})
	})
	host := router.OpenAICompatible("local-image", f.server.URL, "/v1", []string{"gpt-image-1"})
	host.Profiles = []string{router.ProfileImage}
	f.provider.cfg.Router = router.New(host)
	f.provider.cfg.ResolveSelectionV2 = func(context.Context, Subject, string) (wire.SelectionPolicyV2, bool, error) {
		return wire.SelectionPolicyV2{Name: "images", Candidates: []wire.SelectionCandidateV2{{Model: "gpt-image-1", Placement: wire.CandidatePlacementLocal}}}, true, nil
	}
	a := f.provider.StartImageV2WithGuarantees(context.Background(), caller, imageV2Request(wire.Target{SelectionPolicy: "images"}), []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly})
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("local policy: %+v", a)
	}
	_, end := drainImage(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("local end: %+v", end)
	}
}
