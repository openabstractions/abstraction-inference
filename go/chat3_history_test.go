package inference

import (
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

func chat3HistoryFixture(t *testing.T) (*chat3History, time.Time, Subject) {
	t.Helper()
	h, err := newChat3History(time.Minute, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.bindOwner("fixture-job-owner"); err != nil {
		t.Fatal(err)
	}
	return h, time.Unix(0, 0), Subject{Account: "account", Program: "program"}
}

func chat3AdmitHistory(t *testing.T, h *chat3History, now time.Time, subject Subject, epoch, key, request string) *chat3HistoryEntry {
	t.Helper()
	e, owner, outcome, _, err := h.reserve(subject, acceptance.RequestIdentity{Key: key, HistoryEpoch: epoch}, []byte("body"), now)
	if err != nil || !owner || outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("reserve: owner %v outcome %s error %v", owner, outcome, err)
	}
	h.complete(e, wire.ChatAdmission{Outcome: wire.ChatStartOutcomeAccepted, Request: request}, now)
	return e
}

func TestChat3HistoryReplaysAdmissionAndSeparatesSubjects(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	epoch := h.window().HistoryEpoch
	first := chat3AdmitHistory(t, h, now, subject, epoch, "key", "first")
	e, owner, outcome, _, err := h.reserve(subject, acceptance.RequestIdentity{Key: "key", HistoryEpoch: epoch}, []byte("body"), now)
	if err != nil || owner || outcome != wire.ChatStartOutcomeAccepted || e != first || e.admission.Request != "first" {
		t.Fatal("equal identity did not replay its original admission")
	}
	_, _, outcome, _, _ = h.reserve(subject, acceptance.RequestIdentity{Key: "key", HistoryEpoch: epoch}, []byte("different"), now)
	if outcome != wire.ChatStartOutcomeConflict {
		t.Fatal("changed body did not conflict")
	}
	subject.Program = "other-program"
	second := chat3AdmitHistory(t, h, now, subject, epoch, "key", "second")
	if second == first {
		t.Fatal("two attested programs shared an admission")
	}
}

func TestChat3HistoryClosingEpochPreservesPromisedReplayAndFencesNewWork(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	epoch := h.window().HistoryEpoch
	chat3AdmitHistory(t, h, now, subject, epoch, "a", "a")
	chat3AdmitHistory(t, h, now, subject, epoch, "b", "b")
	_, owner, outcome, _, _ := h.reserve(subject, acceptance.RequestIdentity{Key: "rotate", HistoryEpoch: epoch}, []byte("body"), now.Add(time.Second))
	if owner || outcome != wire.ChatStartOutcomeUnknown || h.window().HistoryEpoch == epoch {
		t.Fatal("rotation did not fence the supplied old epoch")
	}
	e, owner, outcome, _, _ := h.reserve(subject, acceptance.RequestIdentity{Key: "a", HistoryEpoch: epoch}, []byte("body"), now.Add(2*time.Second))
	if owner || outcome != wire.ChatStartOutcomeAccepted || e.admission.Request != "a" {
		t.Fatal("closing epoch lost evidence inside its promised horizon")
	}
	_, owner, outcome, _, _ = h.reserve(subject, acceptance.RequestIdentity{Key: "unseen", HistoryEpoch: epoch}, []byte("body"), now.Add(2*time.Second))
	if owner || outcome != wire.ChatStartOutcomeUnknown {
		t.Fatal("closing epoch admitted delayed unseen work")
	}
	_, owner, outcome, _, _ = h.reserve(subject, acceptance.RequestIdentity{Key: "a", HistoryEpoch: epoch}, []byte("body"), now.Add(time.Minute))
	if owner || outcome != wire.ChatStartOutcomeUnknown {
		t.Fatal("expired identity was silently readmitted")
	}
}

func TestChat3HistoryTwoFullTablesRefuseWithRemainingDuration(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	old := h.window().HistoryEpoch
	chat3AdmitHistory(t, h, now, subject, old, "a", "a")
	chat3AdmitHistory(t, h, now, subject, old, "b", "b")
	h.reserve(subject, acceptance.RequestIdentity{Key: "rotate", HistoryEpoch: old}, []byte("body"), now.Add(time.Second))
	current := h.window().HistoryEpoch
	chat3AdmitHistory(t, h, now.Add(2*time.Second), subject, current, "c", "c")
	chat3AdmitHistory(t, h, now.Add(2*time.Second), subject, current, "d", "d")
	_, owner, outcome, retry, err := h.reserve(subject, acceptance.RequestIdentity{Key: "e", HistoryEpoch: current}, []byte("body"), now.Add(3*time.Second))
	if err != nil || owner || outcome != wire.ChatStartOutcomeUnavailable || retry != 57000 {
		t.Fatalf("both full: owner %v outcome %s retry %d error %v", owner, outcome, retry, err)
	}
	if hint := chat3RetryHint(outcome, retry); hint == nil || *hint != 57000 {
		t.Fatal("capacity hint did not retain the remaining duration")
	}
	if h.closing == nil || len(h.closing.entries) != 2 || len(h.open.entries) != 2 {
		t.Fatal("capacity refusal dropped promised evidence")
	}
}

func TestChat3CapacityHintIsOptionalBoundedAndNeverAuthority(t *testing.T) {
	for _, outcome := range []wire.ChatStartOutcome{wire.ChatStartOutcomeAccepted, wire.ChatStartOutcomeForbidden,
		wire.ChatStartOutcomeNotPermitted, wire.ChatStartOutcomeBudgetExceeded, wire.ChatStartOutcomeUnknown} {
		if chat3RetryHint(outcome, 1000) != nil {
			t.Fatalf("%s acquired a capacity hint", outcome)
		}
	}
	for _, remaining := range []int64{-1, 0} {
		if chat3RetryHint(wire.ChatStartOutcomeUnavailable, remaining) != nil {
			t.Fatal("missing capacity estimate fabricated a hint")
		}
	}
	for _, outcome := range []wire.ChatStartOutcome{wire.ChatStartOutcomeUnavailable, wire.ChatStartOutcomeExhausted} {
		for _, remaining := range []int64{1, 86400000, 86400001, 1<<63 - 1} {
			hint := chat3RetryHint(outcome, remaining)
			if hint == nil || *hint != min(remaining, int64(86400000)) {
				t.Fatalf("%s duration %d was not bounded", outcome, remaining)
			}
			admission := wire.ChatAdmission{Outcome: outcome, RetryAfterMs: hint}
			if admission.Refusal != nil {
				t.Fatal("capacity data fabricated an authority refusal")
			}
		}
	}
}

func TestChat3HistoryRestartAndExpiredKeysNeverCreateFreshWork(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	old := h.window().HistoryEpoch
	chat3AdmitHistory(t, h, now, subject, old, "key", "old")
	fresh, err := newChat3History(time.Minute, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.bindOwner(h.window().LogicalOwner); err != nil {
		t.Fatal(err)
	}
	if fresh.window().LogicalOwner != h.window().LogicalOwner {
		t.Fatal("restart replaced persistent logical owner")
	}
	_, owner, outcome, _, _ := fresh.reserve(subject, acceptance.RequestIdentity{Key: "key", HistoryEpoch: old}, []byte("body"), now)
	if owner || outcome != wire.ChatStartOutcomeUnknown || fresh.window().HistoryEpoch == old {
		t.Fatal("restart reused an epoch or resubmitted old work")
	}
	_, owner, outcome, _, _ = h.reserve(subject, acceptance.RequestIdentity{Key: "key", HistoryEpoch: old}, []byte("body"), now.Add(time.Minute))
	if owner || outcome != wire.ChatStartOutcomeUnknown || len(h.open.entries) != 1 {
		t.Fatal("expired open-epoch tombstone was discarded or readmitted")
	}
}

func TestChat3HistoryExplicitAttemptRequiresFailedPredecessor(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	epoch := h.window().HistoryEpoch
	e := chat3AdmitHistory(t, h, now, subject, epoch, "key", "first")
	id := acceptance.RequestIdentity{Key: "key", HistoryEpoch: epoch, Attempt: 1}
	_, owner, outcome, _, _ := h.reserve(subject, id, []byte("body"), now)
	if owner || outcome != wire.ChatStartOutcomeConflict {
		t.Fatal("retry bypassed its live/successful predecessor")
	}
	h.failed(e)
	_, owner, outcome, _, _ = h.reserve(subject, id, []byte("body"), now)
	if !owner || outcome != wire.ChatStartOutcomeAccepted {
		t.Fatal("explicit retry of a failed predecessor was not eligible")
	}
}

func TestChat3HistoryMalformedIdentityCannotConsumeAdmissionCapacity(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	for _, identity := range []acceptance.RequestIdentity{
		{Key: "", HistoryEpoch: h.window().HistoryEpoch},
		{Key: strings.Repeat("k", 129), HistoryEpoch: h.window().HistoryEpoch},
		{Key: string([]byte{255}), HistoryEpoch: h.window().HistoryEpoch},
		{Key: "key", HistoryEpoch: h.window().HistoryEpoch, Attempt: -1},
	} {
		_, owner, outcome, _, err := h.reserve(subject, identity, []byte("body"), now)
		if err != nil || owner || outcome != wire.ChatStartOutcomeInvalid || len(h.open.entries) != 0 {
			t.Fatalf("malformed identity consumed history: owner %v outcome %s error %v", owner, outcome, err)
		}
	}
}
