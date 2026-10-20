package inference

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

const ChatV3Contract = "abstraction.inference/chat@3"
const chat3FrameBytes = 1 << 20

type chat3HistoryKey struct {
	subject Subject
	key     string
	attempt int64
}

type chat3HistoryEntry struct {
	body        []byte
	done        chan struct{}
	admission   wire.ChatAdmission
	until       time.Time
	failed      bool
	running     bool
	remoteEpoch string
}

type chat3Epoch struct {
	id      string
	entries map[chat3HistoryKey]*chat3HistoryEntry
	bytes   int
}

// chat3History implements the Job-owned epoch/equality contract, without a
// durable-work promise. At most one open and one closing table retain bodies.
// Its mutex is never held across admission, transport or a duplicate's wait.
type chat3History struct {
	mu        sync.Mutex
	owner     string
	open      *chat3Epoch
	closing   *chat3Epoch
	retention time.Duration
	entries   int
	bytes     int
}

func freshChat3Epoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func newChat3History(retention time.Duration, operations, retainedDeltas int) (*chat3History, error) {
	id, err := freshChat3Epoch()
	if err != nil {
		return nil, err
	}
	// One entry per operation slot per second of minimum history, capped by
	// the existing retained-delta count. Bodies also share at most one frame
	// per operation slot, so large inputs cannot turn the count into GiB of
	// default history. These are source-owned admission capacities, not rates.
	if operations < 1 {
		operations = 1
	}
	if retainedDeltas < 1 {
		retainedDeltas = 1
	}
	seconds := int64(retention / time.Second)
	if retention%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	count := retainedDeltas
	if seconds <= int64(count/operations) {
		count = operations * int(seconds)
	}
	slots := min(operations, count)
	if slots > int(^uint(0)>>1)/chat3FrameBytes {
		return nil, errors.New("inference: history frame capacity overflows native size")
	}
	return &chat3History{
		open:      &chat3Epoch{id: id, entries: map[chat3HistoryKey]*chat3HistoryEntry{}},
		retention: retention, entries: count, bytes: slots * chat3FrameBytes}, nil
}

func (h *chat3History) bindOwner(owner string) error {
	// This is the existing acceptance provider's configured owner grammar.
	if owner == "" || len(owner) > 1024 || !utf8.ValidString(owner) {
		return errors.New("inference: trusted restart-stable chat logical owner required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.owner != "" && h.owner != owner {
		return errors.New("inference: chat3 logical owner already bound")
	}
	h.owner = owner
	return nil
}

func (h *chat3History) bound() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.owner != ""
}

func (h *chat3History) window() acceptance.HistoryWindow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return acceptance.HistoryWindow{LogicalOwner: h.owner, HistoryEpoch: h.open.id,
		MinimumRetentionMs: h.retention.Milliseconds(), ResultRetentionMs: h.retention.Milliseconds()}
}

func (h *chat3History) closingHorizon() time.Time {
	var last time.Time
	if h.closing != nil {
		for _, e := range h.closing.entries {
			// An in-flight admission owns a pending result and cannot be dropped.
			if e.running || e.until.IsZero() {
				return time.Time{}
			}
			if e.until.After(last) {
				last = e.until
			}
		}
	}
	return last
}

// reserve returns either the one new admission owner, a retained duplicate,
// or a final no-admission outcome. Zero retry means no known duration.
func (h *chat3History) reserve(subject Subject, identity acceptance.RequestIdentity, body []byte, now time.Time) (*chat3HistoryEntry, bool, wire.ChatStartOutcome, int64, error) {
	if identity.Key == "" || len(identity.Key) > 128 || !utf8.ValidString(identity.Key) || identity.Attempt < 0 {
		return nil, false, wire.ChatStartOutcomeInvalid, 0, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing != nil {
		last := h.closingHorizon()
		if !last.IsZero() && !now.Before(last) {
			h.closing = nil
		}
	}
	var table *chat3Epoch
	switch identity.HistoryEpoch {
	case h.open.id:
		table = h.open
	default:
		if h.closing != nil && identity.HistoryEpoch == h.closing.id {
			table = h.closing
		}
	}
	if table == nil {
		return nil, false, wire.ChatStartOutcomeUnknown, 0, nil
	}
	key := chat3HistoryKey{subject: subject, key: identity.Key, attempt: identity.Attempt}
	if e := table.entries[key]; e != nil {
		if !e.running && !e.until.IsZero() && !now.Before(e.until) {
			return nil, false, wire.ChatStartOutcomeUnknown, 0, nil
		}
		if !bytes.Equal(e.body, body) {
			return nil, false, wire.ChatStartOutcomeConflict, 0, nil
		}
		return e, false, wire.ChatStartOutcomeAccepted, 0, nil
	}
	if table != h.open {
		return nil, false, wire.ChatStartOutcomeUnknown, 0, nil
	}
	if identity.Attempt > 0 {
		previous := table.entries[chat3HistoryKey{subject: subject, key: identity.Key, attempt: identity.Attempt - 1}]
		if previous == nil {
			return nil, false, wire.ChatStartOutcomeUnknown, 0, nil
		}
		if !previous.failed || !bytes.Equal(previous.body, body) {
			return nil, false, wire.ChatStartOutcomeConflict, 0, nil
		}
	}
	if len(table.entries) >= h.entries || len(body) > h.bytes-table.bytes {
		if h.closing != nil {
			last := h.closingHorizon()
			var retry int64
			if !last.IsZero() && last.After(now) {
				remaining := last.Sub(now)
				retry = int64(remaining / time.Millisecond)
				if remaining%time.Millisecond != 0 {
					retry++
				}
			}
			return nil, false, wire.ChatStartOutcomeUnavailable, retry, nil
		}
		id, err := freshChat3Epoch()
		if err != nil {
			return nil, false, wire.ChatStartOutcomeUnavailable, 0, err
		}
		h.closing, h.open = table, &chat3Epoch{id: id, entries: map[chat3HistoryKey]*chat3HistoryEntry{}}
		// The request's old epoch was fenced by rotation. Never substitute the
		// fresh epoch or automatically start under a new identity.
		return nil, false, wire.ChatStartOutcomeUnknown, 0, nil
	}
	e := &chat3HistoryEntry{body: bytes.Clone(body), done: make(chan struct{})}
	if identity.Attempt > 0 {
		e.remoteEpoch = table.entries[chat3HistoryKey{subject: subject, key: identity.Key, attempt: identity.Attempt - 1}].remoteEpoch
	}
	table.entries[key], table.bytes = e, table.bytes+len(body)
	return e, true, wire.ChatStartOutcomeAccepted, 0, nil
}

func (h *chat3History) complete(e *chat3HistoryEntry, admission wire.ChatAdmission, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e.admission = admission
	if admission.Outcome != wire.ChatStartOutcomeAccepted {
		e.running = false
	}
	if !e.running && e.until.IsZero() {
		e.until = now.Add(h.retention)
	}
	close(e.done)
}

// lookup answers retained evidence before any admission preflight (JOB-A16).
// A nil entry with accepted means an eligible new key, not accepted work.
// reserve repeats the decision under the same mutex at the commit boundary.
func (h *chat3History) lookup(subject Subject, identity acceptance.RequestIdentity, body []byte, now time.Time) (*chat3HistoryEntry, wire.ChatStartOutcome) {
	if identity.Key == "" || len(identity.Key) > 128 || !utf8.ValidString(identity.Key) || identity.Attempt < 0 {
		return nil, wire.ChatStartOutcomeInvalid
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	table := h.open
	if identity.HistoryEpoch != table.id {
		table = h.closing
		if table == nil || identity.HistoryEpoch != table.id {
			return nil, wire.ChatStartOutcomeUnknown
		}
	}
	key := chat3HistoryKey{subject: subject, key: identity.Key, attempt: identity.Attempt}
	if entry := table.entries[key]; entry != nil {
		if !entry.running && !entry.until.IsZero() && !now.Before(entry.until) {
			return nil, wire.ChatStartOutcomeUnknown
		}
		if !bytes.Equal(entry.body, body) {
			return nil, wire.ChatStartOutcomeConflict
		}
		return entry, wire.ChatStartOutcomeAccepted
	}
	if table != h.open {
		return nil, wire.ChatStartOutcomeUnknown
	}
	if identity.Attempt > 0 {
		previous := table.entries[chat3HistoryKey{subject: subject, key: identity.Key, attempt: identity.Attempt - 1}]
		if previous == nil {
			return nil, wire.ChatStartOutcomeUnknown
		}
		if !previous.failed || !bytes.Equal(previous.body, body) {
			return nil, wire.ChatStartOutcomeConflict
		}
	}
	return nil, wire.ChatStartOutcomeAccepted
}

// abort publishes a preaccept refusal to its concurrent waiters and removes
// only this uncommitted reservation. It creates no tombstone or sealed result.
func (h *chat3History) abort(e *chat3HistoryEntry, admission wire.ChatAdmission) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, table := range []*chat3Epoch{h.open, h.closing} {
		if table == nil {
			continue
		}
		for key, entry := range table.entries {
			if entry == e {
				delete(table.entries, key)
				table.bytes -= len(entry.body)
			}
		}
	}
	e.admission = admission
	e.running = false
	close(e.done)
}

func (h *chat3History) begin(e *chat3HistoryEntry) {
	h.mu.Lock()
	e.running = true
	h.mu.Unlock()
}

func (h *chat3History) finish(e *chat3HistoryEntry, failed bool, now time.Time) {
	h.mu.Lock()
	e.running, e.failed, e.until = false, failed, now.Add(h.retention)
	h.mu.Unlock()
}

func (h *chat3History) failed(e *chat3HistoryEntry) {
	h.mu.Lock()
	e.failed = true
	h.mu.Unlock()
}

func (h *chat3History) remoteEpoch(e *chat3HistoryEntry) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return e.remoteEpoch
}
func (h *chat3History) bindRemoteEpoch(e *chat3HistoryEntry, epoch string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e.remoteEpoch == "" {
		e.remoteEpoch = epoch
	}
}

// chat3RetryHint is record-level data for a non-authority capacity failure.
// It never fabricates an authority RefusalDetail or grants permission to retry.
func chat3RetryHint(outcome wire.ChatStartOutcome, remainingMs int64) *int64 {
	if remainingMs <= 0 || outcome != wire.ChatStartOutcomeUnavailable && outcome != wire.ChatStartOutcomeExhausted {
		return nil
	}
	hint := min(remainingMs, int64(86400000))
	return &hint
}
