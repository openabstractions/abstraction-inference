package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func selectionPolicy(_ context.Context, _ Subject, _ string) (wire.SelectionPolicyV2, bool, error) {
	return wire.SelectionPolicyV2{Name: "offline", Candidates: []wire.SelectionCandidateV2{
		{Model: "cloud-model", Placement: wire.CandidatePlacementHosted, Credential: "cloudcred"},
		{Model: "local-model", Placement: wire.CandidatePlacementLocal}}}, true, nil
}

func selectionRequest() wire.RequestV2 {
	return wire.RequestV2{Target: wire.Target{SelectionPolicy: "offline"}, Content: wire.RequestContent{
		Messages:   []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
		Guarantees: []wire.RequestGuarantee{}}}
}

func TestChatV1RequestRemainsStrict(t *testing.T) {
	request := RequestFromContent("old-model", selectionRequest().Content)
	encoded := wire.Encode(&request)
	decoded, err := wire.Decode(encoded)
	if err != nil || decoded.Model != "old-model" || len(decoded.Messages) != 1 {
		t.Fatalf("chat@1 request round trip: %+v %v", decoded, err)
	}
	if _, err := wire.Decode([]byte(`{"model":"old-model","messages":[],"guarantees":[],"target":{"model":"new-model"}}`)); err == nil {
		t.Fatal("chat@1 accepted a chat@2 target field")
	}
}

func TestChatV2TargetIsExclusive(t *testing.T) {
	provider, err := New(Config{Router: router.New(), Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	for _, target := range []wire.Target{{}, {Model: "a", SelectionPolicy: "b"}} {
		req := selectionRequest()
		req.Target = target
		got := provider.StartV2(context.Background(), caller, req)
		if got.Outcome != wire.StartOutcomeInvalid || got.SelectionReason != nil || got.PreferredModel != "" {
			t.Fatalf("target %+v: %+v", target, got)
		}
	}
}

func TestChatV2PolicyRejectsInlineOverrides(t *testing.T) {
	provider, err := New(Config{Router: router.New(), Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	for _, mutate := range []func(*wire.RequestV2){
		func(req *wire.RequestV2) { req.Content.Credential = "cloudcred" },
		func(req *wire.RequestV2) {
			req.Content.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}
		},
	} {
		req := selectionRequest()
		mutate(&req)
		got := provider.StartV2(context.Background(), caller, req)
		if got.Outcome != wire.StartOutcomeInvalid || got.Reason != "selection:override" {
			t.Fatalf("inline override: %+v", got)
		}
	}
}

func TestChatV2CloudListingUnavailableSelectsOllama(t *testing.T) {
	var cloudPosts, ollamaPosts atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		cloudPosts.Add(1)
	}))
	defer cloud.Close()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"local-model"}]}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/v1/chat/completions":
			ollamaPosts.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("local fallback received hosted credential header")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ollama.Close()
	r := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"), router.Ollama(ollama.URL))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	provider, err := New(Config{Router: r, ResolveSelectionV2: func(context.Context, Subject, string) (wire.SelectionPolicyV2, bool, error) {
		return wire.SelectionPolicyV2{Name: "offline", Candidates: []wire.SelectionCandidateV2{
			{Model: "cloud-model", Placement: wire.CandidatePlacementHosted, Credential: "cloudcred"},
			{Model: "local-model", Placement: wire.CandidatePlacementLocal}}}, true, nil
	}, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	got := provider.StartV2(context.Background(), caller, selectionRequest())
	if got.Outcome != wire.StartOutcomeAccepted || got.Host != "ollama" || got.Model != "local-model" || got.PreferredModel != "cloud-model" ||
		got.SelectionReason == nil || *got.SelectionReason != wire.SelectionReasonPreferredRouteUnavailable {
		t.Fatalf("fallback admission: %+v", got)
	}
	_, reply, _ := drain(t, provider, caller, got.Operation)
	if reply.Host != "ollama" || reply.Model != "local-model" || cloudPosts.Load() != 0 || ollamaPosts.Load() != 1 {
		t.Fatalf("actual route: reply %+v, cloud posts %d, ollama posts %d", reply, cloudPosts.Load(), ollamaPosts.Load())
	}
}

func TestChatV2NeverFallsBackOnCredentialOrUnknownListing(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer cloud.Close()
			local := router.OpenAICompatible("local", "http://127.0.0.1:1", "/v1", []string{"local-model"})
			r := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"), local)
			r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
			provider, err := New(Config{Router: r, ResolveSelectionV2: func(context.Context, Subject, string) (wire.SelectionPolicyV2, bool, error) {
				return wire.SelectionPolicyV2{Name: "offline", Candidates: []wire.SelectionCandidateV2{
					{Model: "cloud-model", Placement: wire.CandidatePlacementHosted, Credential: "cloudcred"},
					{Model: "local-model", Placement: wire.CandidatePlacementLocal}}}, true, nil
			}, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
				Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
					return nil, "applied"
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			got := provider.StartV2(context.Background(), caller, selectionRequest())
			if got.Outcome != wire.StartOutcomeNoHost || got.Host != "" || got.SelectionReason != nil {
				t.Fatalf("status %d: %+v", status, got)
			}
		})
	}
}

func TestChatV2NeverFallsBackAfterAdmissionGateOrPartialSend(t *testing.T) {
	for _, tc := range []struct {
		name            string
		denyRight       bool
		denyCredential  bool
		partialUpstream bool
		want            wire.StartOutcome
	}{
		{"rights", true, false, false, wire.StartOutcomeNotPermitted},
		{"credential", false, true, false, wire.StartOutcomeNotPermitted},
		{"partial", false, false, true, wire.StartOutcomeAccepted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cloudPosts, localPosts atomic.Int32
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprint(w, `{"data":[{"id":"cloud-model"}]}`)
					return
				}
				cloudPosts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.partialUpstream {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\nevent: error\ndata: {\"error\":{\"message\":\"failed\"}}\n\n")
				}
			}))
			defer cloud.Close()
			local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					localPosts.Add(1)
				}
			}))
			defer local.Close()
			r := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"),
				router.OpenAICompatible("local", local.URL, "/v1", []string{"local-model"}))
			r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
			provider, err := New(Config{Router: r, ResolveSelectionV2: selectionPolicy,
				Decide: func(_ context.Context, _ Subject, _ string, _ string) (string, error) {
					if tc.denyRight {
						return "denied", nil
					}
					return "permitted", nil
				},
				Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
					if tc.denyCredential {
						return nil, "not_permitted"
					}
					return nil, "applied"
				}})
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			got := provider.StartV2(context.Background(), caller, selectionRequest())
			if got.Outcome != tc.want {
				t.Fatalf("admission %+v, want %s", got, tc.want)
			}
			if got.Outcome == wire.StartOutcomeAccepted {
				if got.SelectionReason == nil || *got.SelectionReason != wire.SelectionReasonPreferred {
					t.Fatalf("preferred admission: %+v", got)
				}
				_, reply, deltas := drain(t, provider, caller, got.Operation)
				if reply.Outcome == wire.ReplyOutcomeCompleted || len(deltas) < 2 {
					t.Fatalf("partial failure: %+v, deltas %+v", reply, deltas)
				}
			} else if got.SelectionReason != nil || got.PreferredModel != "" {
				t.Fatalf("refusal disclosed selection: %+v", got)
			}
			if localPosts.Load() != 0 {
				t.Fatalf("local retry after %s: %d", tc.name, localPosts.Load())
			}
			if tc.partialUpstream && cloudPosts.Load() != 1 || !tc.partialUpstream && cloudPosts.Load() != 0 {
				t.Fatalf("cloud posts after %s: %d", tc.name, cloudPosts.Load())
			}
		})
	}
}

func TestChatV2CancellationStopsListingAndFallback(t *testing.T) {
	started := make(chan struct{}, 1)
	var localPosts atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer cloud.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			localPosts.Add(1)
		}
	}))
	defer local.Close()
	r := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"),
		router.OpenAICompatible("local", local.URL, "/v1", []string{"local-model"}))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	provider, err := New(Config{Router: r, ResolveSelectionV2: selectionPolicy,
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan wire.AdmissionV2, 1)
	go func() { done <- provider.StartV2(ctx, caller, selectionRequest()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hosted listing did not start")
	}
	cancel()
	select {
	case admission := <-done:
		if admission.Outcome != wire.StartOutcomeUnavailable || admission.SelectionReason != nil {
			t.Fatalf("cancelled selection: %+v", admission)
		}
	case <-time.After(time.Second):
		t.Fatal("selection ignored cancellation")
	}
	if localPosts.Load() != 0 {
		t.Fatalf("local request after cancellation: %d", localPosts.Load())
	}
}
