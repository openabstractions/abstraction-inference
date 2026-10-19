package inference

import (
	"context"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func TestNativeImageCancellationAfterAcceptedStartCancelsKnownOperation(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	stop()
	var cancelled string
	if !cancelledAfterNativeStart(ctx, wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "known-provider-operation"}, func(id string) { cancelled = id }) {
		t.Fatal("cancelled context continued to Observe")
	}
	if cancelled != "known-provider-operation" {
		t.Fatalf("cancelled operation %q", cancelled)
	}
	if cancelledAfterNativeStart(context.Background(), wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "live"}, func(string) { t.Fatal("live operation cancelled") }) {
		t.Fatal("live context refused")
	}
}
