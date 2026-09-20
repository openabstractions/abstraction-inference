package client

import "testing"

func TestRequestGuaranteePublicChoicesUsePublishedWireWords(t *testing.T) {
	want := []struct {
		value RequestGuarantee
		word  string
	}{
		{RequestGuaranteeLocalOnly, "abstraction.inference/local-only@1"},
		{RequestGuaranteeHostedAllowed, "abstraction.inference/hosted-allowed@1"},
	}
	if values := RequestGuaranteeValues(); len(values) != len(want) {
		t.Fatalf("RequestGuaranteeValues returned %d values", len(values))
	}
	for i, choice := range want {
		if got := string(choice.value); got != choice.word {
			t.Fatalf("choice %d wire word %q, want %q", i, got, choice.word)
		}
	}
	future := RequestGuarantee("example.runtime/private-routing@1")
	if future.Known() || string(future) != "example.runtime/private-routing@1" {
		t.Fatalf("future request guarantee was not retained: %q", future)
	}
}
