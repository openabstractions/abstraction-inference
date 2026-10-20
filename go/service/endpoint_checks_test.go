package service

import (
	"bytes"
	"encoding/json"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func TestHostEndpointProducerChecksAreIndependentAndOptional(t *testing.T) {
	frame := []byte(`{"version":1,"service":"abstraction.facade/endpoint@2","method":"Describe","arguments":{}}`)
	h := &Host{}
	read := func() wire.EndpointDescription {
		t.Helper()
		reply, err := h.serveEndpoint(frame)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Payload struct{ Value json.RawMessage }
		}
		if err := json.Unmarshal(reply, &envelope); err != nil {
			t.Fatal(err)
		}
		var description wire.EndpointDescription
		if err := wire.FromJSON(envelope.Payload.Value, &description); err != nil {
			t.Fatal(err)
		}
		return description
	}
	if description := read(); len(description.Checks) != 0 || description.Outcome != wire.EndpointDescriptionOutcomeDescribed {
		t.Fatal(description)
	}
	calls := 0
	check := wire.EndpointProducerCheck{Kind: "fixture/engine", Copy: "release", State: wire.EndpointProducerCheckStateUnverified, Reason: "engine:custom_path", Checked: "2026-11-02T00:00:00.000000Z"}
	h.ProducerChecks = func() []wire.EndpointProducerCheck { calls++; return []wire.EndpointProducerCheck{check} }
	if description := read(); len(description.Checks) != 1 || description.Checks[0] != check || len(description.Services) != 0 {
		t.Fatal(description)
	}
	legacy := bytes.Replace(frame, []byte("endpoint@2"), []byte("endpoint@1"), 1)
	if _, err := h.serveEndpoint(legacy); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("legacy description read producer checks", calls)
	}
}
