package service

import (
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"strings"
	"testing"
)

func TestRequestsLegacyCustomHostIsUnavailable(t *testing.T) {
	r := &operatorReceiver{receiver: &receiver{host: &Host{Operator: &legacyOperatorOnly{}}}}
	p, err := r.Requests(7, 64)
	if err != nil || p.Outcome != wire.AuditOutcomeUnavailable || len(p.Entries) != 0 || p.Next != 7 || p.AtEnd {
		t.Fatal(p, err)
	}
}

func TestRequestsPageAggregateRefusesWholeFramedReply(t *testing.T) {
	for _, size := range []int{16, 4100, 5000} {
		p := wire.RequestPage{Outcome: wire.AuditOutcomePage, Entries: []wire.RequestEntry{}, Next: 256, AtEnd: true}
		for i := 0; i < 256; i++ {
			p.Entries = append(p.Entries, wire.RequestEntry{Entry: wire.AuditEntry{Sequence: int64(i), Route: wire.AuditRouteNative, Outcome: "completed", Reason: strings.Repeat("x", size)}})
		}
		got, err := requestPage(p, 0, 256)
		if err != nil {
			t.Fatal(err)
		}
		if size == 16 {
			if got.Outcome != wire.AuditOutcomePage || len(got.Entries) != 256 {
				t.Fatal(got.Outcome, len(got.Entries))
			}
		} else if got.Outcome != wire.AuditOutcomeUnavailable || len(got.Entries) != 0 || got.Next != 0 || got.AtEnd {
			t.Fatal(got.Outcome, len(got.Entries), got.Next, got.AtEnd)
		}
	}
	if _, err := wire.NewOperatorClient(&requestReplyCapture{}).Hosts(); err == nil {
		t.Fatal("preflight invoked unrelated method")
	}
}
