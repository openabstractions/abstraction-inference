package inference

import (
	"context"
	"sync"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type remoteLiveError struct {
	outcome wire.ReplyOutcome
	reason  string
}

func (e *remoteLiveError) Error() string { return "remote live: operation failed" }

type remoteLiveBackend struct {
	transport listen.FrameClient
	operation string

	writeMu    sync.Mutex
	readMu     sync.Mutex
	stateMu    sync.Mutex
	next       int64
	cursor     int64
	queue      []liveBackendEvent
	closed     bool
	terminal   bool
	cancelOnce sync.Once
}

func dialRemoteLiveBackend(ctx context.Context, op *operation, req wire.LiveRequest) (liveBackend, error) {
	transport, ok := op.host.RemoteTransport()
	if !ok {
		return nil, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:transport"}
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Extensions = cloneStrings(req.Extensions)
	if forwarded.Extensions == nil {
		forwarded.Extensions = map[string]string{}
	}
	forwarded.Extensions[ClaimExtension] = op.subject.Program
	admission, err := wire.NewLiveClient(transport.WithContext(ctx)).Start(forwarded)
	if err != nil {
		return nil, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:unreachable"}
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return nil, &remoteLiveError{outcome: outcome, reason: "remote:" + admission.Reason}
	}
	if admission.Operation == "" {
		return nil, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
	}
	return &remoteLiveBackend{transport: transport, operation: admission.Operation}, nil
}

func (b *remoteLiveBackend) append(ctx context.Context, audio []byte) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.stateMu.Lock()
	if b.closed || b.terminal {
		b.stateMu.Unlock()
		return liveError(liveBackendInvalidState)
	}
	sequence := b.next
	b.stateMu.Unlock()
	result, err := wire.NewLiveClient(b.transport.WithContext(ctx)).Append(b.operation, sequence, audio)
	if err != nil {
		b.cancelRemote()
		return liveErrorForContext(ctx, liveBackendUnavailable)
	}
	if result.Outcome != wire.LiveInputOutcomeAccepted || result.NextSequence != sequence+1 {
		b.cancelRemote()
		return liveError(liveBackendMalformed)
	}
	b.stateMu.Lock()
	b.next = result.NextSequence
	b.stateMu.Unlock()
	return nil
}

func (b *remoteLiveBackend) commit(ctx context.Context) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.stateMu.Lock()
	if b.closed || b.terminal {
		b.stateMu.Unlock()
		return liveError(liveBackendInvalidState)
	}
	next := b.next
	b.stateMu.Unlock()
	result, err := wire.NewLiveClient(b.transport.WithContext(ctx)).Commit(b.operation)
	if err != nil {
		b.cancelRemote()
		return liveErrorForContext(ctx, liveBackendUnavailable)
	}
	if result.Outcome != wire.LiveInputOutcomeAccepted || result.NextSequence != next {
		b.cancelRemote()
		return liveError(liveBackendMalformed)
	}
	return nil
}

func (b *remoteLiveBackend) read(ctx context.Context) (liveBackendEvent, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	for {
		if len(b.queue) > 0 {
			event := b.queue[0]
			b.queue = b.queue[1:]
			return event, nil
		}
		b.stateMu.Lock()
		if b.closed || b.terminal {
			b.stateMu.Unlock()
			return liveBackendEvent{}, liveError(liveBackendInvalidState)
		}
		cursor := b.cursor
		b.stateMu.Unlock()
		page, err := wire.NewLiveClient(b.transport.WithContext(ctx)).Observe(b.operation, cursor, 256, 65536, remoteObserveWaitMS)
		if err != nil {
			b.cancelRemote()
			return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:unreachable"}
		}
		if page.Outcome != wire.PageOutcomePage || page.Next != cursor+int64(len(page.Deltas)) {
			b.cancelRemote()
			return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
		}
		queue := make([]liveBackendEvent, 0, len(page.Deltas))
		terminal := false
		for i, delta := range page.Deltas {
			if delta.Sequence != cursor+int64(i) || terminal {
				b.cancelRemote()
				return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
			}
			switch delta.Kind {
			case wire.DeltaKindAudio:
				if delta.Audio == nil || len(delta.Audio.Data) == 0 || len(delta.Audio.Data) > maxLiveAudioBytes {
					b.cancelRemote()
					return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:audio"}
				}
				queue = append(queue, liveBackendEvent{kind: liveBackendEventAudio, audio: append([]byte(nil), delta.Audio.Data...)})
			case wire.DeltaKindTranscript:
				if delta.Transcript == nil || len(delta.Transcript.Text) > maxLiveTranscriptBytes || delta.Transcript.Text == "" && !delta.Transcript.IsFinal {
					b.cancelRemote()
					return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:transcript"}
				}
				queue = append(queue, liveBackendEvent{kind: liveBackendEventTranscript, transcript: delta.Transcript.Text, final: delta.Transcript.IsFinal})
			case wire.DeltaKindEnd:
				if delta.LiveEnd == nil || i != len(page.Deltas)-1 || !page.AtEnd || !delta.LiveEnd.Outcome.Known() {
					b.cancelRemote()
					return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
				}
				copy := *delta.LiveEnd
				if copy.Outcome != wire.ReplyOutcomeCompleted && (copy.Delivery.Digest != "" || copy.Delivery.MediaType != "" || copy.Delivery.Size != 0 || copy.Delivery.Location != "") {
					b.cancelRemote()
					return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:delivery"}
				}
				queue = append(queue, liveBackendEvent{kind: liveBackendEventTerminal, final: true, outcome: copy.Outcome.String(), reason: copy.Reason,
					usage: liveBackendUsage{inputTokens: copy.Usage.Input, outputTokens: copy.Usage.Output}, remoteEnd: &copy})
				terminal = true
			default:
				b.cancelRemote()
				return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
			}
		}
		if page.AtEnd && !terminal {
			b.cancelRemote()
			return liveBackendEvent{}, &remoteLiveError{outcome: wire.ReplyOutcomeUnavailable, reason: "remote:malformed"}
		}
		b.stateMu.Lock()
		b.cursor = page.Next
		if terminal {
			b.terminal = true
		}
		b.stateMu.Unlock()
		b.queue = queue
	}
}

func (b *remoteLiveBackend) close() {
	b.stateMu.Lock()
	terminal := b.terminal
	b.closed = true
	b.stateMu.Unlock()
	if !terminal {
		b.cancelRemote()
	}
}

func (b *remoteLiveBackend) cancelRemote() {
	b.cancelOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = wire.NewLiveClient(b.transport.WithContext(ctx)).Cancel(b.operation)
	})
}
