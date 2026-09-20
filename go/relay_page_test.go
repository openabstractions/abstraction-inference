package inference

import (
	"context"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"math"
	"strings"
	"testing"
)

func TestRelayChatPagePreservesContinuityAndBounds(t *testing.T) {
	part := wire.Delta{Sequence: 3, Kind: wire.DeltaKindPart, Part: &wire.Part{Kind: wire.PartKindText}}
	end := wire.Delta{Sequence: 4, Kind: wire.DeltaKindEnd, End: &wire.Reply{Outcome: wire.ReplyOutcomeCompleted, Message: wire.Message{Role: wire.RoleAssistant, Parts: []wire.Part{}}, StopReason: wire.StopReasonEnd}}
	valid := wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{part, end}, Next: 5, AtEnd: true}
	if !validateRelayChatPage(valid, 3) {
		t.Fatal("valid sequential terminal page refused")
	}
	tests := map[string]wire.DeltaPage{
		"replay":            {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{part}, Next: 3},
		"gap":               {Outcome: wire.PageOutcomeGap, Next: 7},
		"skipped":           {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{end}, Next: 4, AtEnd: true},
		"missing terminal":  {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{part}, Next: 4, AtEnd: true},
		"missing payload":   {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 3, Kind: wire.DeltaKindPart}}, Next: 4},
		"wrong profile":     {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 3, Kind: wire.DeltaKindAudio}}, Next: 4},
		"unmarked terminal": {Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{part, end}, Next: 5},
	}
	for name, page := range tests {
		t.Run(name, func(t *testing.T) {
			if validateRelayChatPage(page, 3) {
				t.Fatal("invalid page accepted")
			}
		})
	}
	end.Sequence = 3
	part.Sequence = 4
	if validateRelayChatPage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{end, part}, Next: 5, AtEnd: true}, 3) {
		t.Fatal("data after terminal accepted")
	}
	if validateRelayChatPage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: math.MaxInt64, Kind: wire.DeltaKindPart, Part: &wire.Part{Kind: wire.PartKindText}}}, Next: math.MinInt64}, math.MaxInt64) {
		t.Fatal("overflow accepted")
	}
	huge := wire.Delta{Sequence: 0, Kind: wire.DeltaKindPart, Part: &wire.Part{Kind: wire.PartKindText, Text: strings.Repeat("x", 65536)}}
	if !validateRelayChatPage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{huge}, Next: 1}, 0) {
		t.Fatal("contract first-large-delta exception refused")
	}
	huge.Sequence = 1
	if validateRelayChatPage(wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 0, Kind: wire.DeltaKindPart, Part: &wire.Part{Kind: wire.PartKindText}}, huge}, Next: 2}, 0) {
		t.Fatal("page byte bound ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if idleRelay(ctx) {
		t.Fatal("cancelled idle relay continued")
	}
}
