package client

import (
	"context"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"testing"
	"time"
)

func TestOperationPageValidationAndWaitBudget(t *testing.T) {
	end := Delta{Sequence: 4, Kind: wire.DeltaKindEnd, TranscriptionEnd: &TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted}}
	valid := func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.TranscriptionEnd != nil }
	for _, tc := range []struct {
		name      string
		page      DeltaPage
		wantError bool
	}{
		{"terminal", DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []Delta{end}, Next: 5, AtEnd: true}, false},
		{"missing-terminal-flag", DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []Delta{end}, Next: 5}, true},
		{"wrong-profile", DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []Delta{{Sequence: 4, Kind: wire.DeltaKindEnd, End: &Reply{}}}, Next: 5, AtEnd: true}, true},
		{"cursor-skips", DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []Delta{end}, Next: 6, AtEnd: true}, true},
		{"gap", DeltaPage{Outcome: wire.PageOutcomeGap, Next: 8}, false},
		{"false-gap", DeltaPage{Outcome: wire.PageOutcomeGap, Next: 4}, true},
		{"already-ended", DeltaPage{Outcome: wire.PageOutcomePage, Next: 4, AtEnd: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := observeOperation(context.Background(), listen.FrameClient{Timeout: time.Second}, 4, 10, 65536, 2000, valid, func(tr listen.FrameClient) (DeltaPage, error) {
				if tr.Timeout != 3*time.Second {
					t.Fatalf("budget %v", tr.Timeout)
				}
				return tc.page, nil
			})
			if errors.Is(err, ErrInconsistent) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestInvalidObservationMakesNoCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		ctx    context.Context
		cursor int64
	}{{ctx, 0}, {context.Background(), -1}} {
		_, err := observeOperation(tc.ctx, listen.FrameClient{}, tc.cursor, 1, 1, 0, func(Delta) bool { return true }, func(listen.FrameClient) (DeltaPage, error) { t.Fatal("called transport"); return DeltaPage{}, nil })
		if err == nil {
			t.Fatal("invalid observation accepted")
		}
	}
}
