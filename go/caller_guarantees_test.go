package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestCallerGuaranteesHostedPolicyRefusesBeforeEffects(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("hosted survey or dispatch") }))
	defer cloud.Close()
	r := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		t.Error("survey credential")
		return nil, nil
	})
	p, err := New(Config{Router: r, ResolveSelectionV2: selectionPolicy,
		Decide: func(context.Context, Subject, string, string) (string, error) {
			t.Error("rights admission")
			return "permitted", nil
		},
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			t.Error("credential application")
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	local := []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	for _, got := range []wire.AdmissionV2{
		p.StartV2WithGuarantees(context.Background(), caller, selectionRequest(), local),
		p.StartImageV2WithGuarantees(context.Background(), caller, imageV2Request(wire.Target{SelectionPolicy: "offline"}), local),
	} {
		if got.Outcome != wire.StartOutcomeNoHost || got.Reason != "selection:placement" || got.Operation != "" {
			t.Fatalf("privacy refusal: %+v", got)
		}
	}
}

func TestCallerGuaranteesIntersectAndPreserveOldPolicyContract(t *testing.T) {
	local := []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	for _, tc := range []struct {
		extras, existing []wire.RequestGuarantee
		credential       string
		placement        ExecutionPlacement
		want             string
	}{
		{nil, []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, "cloud", ExecutionRemote, ""},
		{local, nil, "", ExecutionAny, ""},
		{local, local, "", ExecutionLocal, ""},
		{local, nil, "", ExecutionRemote, "guarantees:conflict"},
		{local, nil, "cloud", ExecutionAny, "guarantees:conflict"},
		{local, []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, "", ExecutionAny, "guarantees:conflict"},
		{[]wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, nil, "", ExecutionAny, "guarantees:caller"},
		{append(append([]wire.RequestGuarantee{}, local...), local...), nil, "", ExecutionAny, "guarantees:caller"},
	} {
		ctx, reason := callerGuarantees(WithExecutionPlacement(context.Background(), tc.placement), tc.extras, tc.existing, tc.credential)
		if reason != tc.want {
			t.Fatalf("%+v: %q", tc, reason)
		}
		if reason == "" && len(tc.extras) != 0 && executionPlacementOf(ctx) != ExecutionLocal {
			t.Fatal("restriction erased")
		}
	}
	p, err := New(Config{Router: router.New(), ResolveSelectionV2: selectionPolicy, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil }, Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
		return nil, "applied"
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	req := selectionRequest()
	req.Content.Guarantees = local
	if got := p.StartV2(context.Background(), caller, req); got.Reason != "selection:override" {
		t.Fatalf("old Start changed: %+v", got)
	}
	image := imageV2Request(wire.Target{SelectionPolicy: "offline"})
	image.Content.Guarantees = local
	if got := p.StartImageV2(context.Background(), caller, image); got.Reason != "selection:override" {
		t.Fatalf("old image Start changed: %+v", got)
	}
}
