package inference

import (
	"context"
	"net/http"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// LiveDialer opens a vendor realtime session after core has admitted the
// operation and protected the supplied HTTP client against credential redirects.
// Implementations belong in provider adapters, not in this service module.
type LiveDialer func(context.Context, *http.Client, string, string, string, wire.LiveFormat, map[string]string) (LiveConnection, error)

// LiveConnection is the provider-neutral local streaming seam. The service
// owns the lifecycle and closes the connection on cancellation or completion.
type LiveConnection interface {
	Append(context.Context, []byte) error
	Commit(context.Context) error
	Read(context.Context) (LiveEvent, error)
	Close()
}

type LiveEventKind uint8

const (
	LiveEventAudio LiveEventKind = iota + 1
	LiveEventTranscript
	LiveEventTerminal
)

// LiveOutcome describes terminal vendor-session status. The service maps it
// to its public reply outcome; numeric values never travel on the wire.
type LiveOutcome uint8

const (
	LiveCompleted LiveOutcome = iota + 1
	LiveCancelled
	LiveFailed
	LiveIncomplete
)

func (o LiveOutcome) word() string {
	switch o {
	case LiveCompleted:
		return "completed"
	case LiveCancelled:
		return "cancelled"
	case LiveFailed:
		return "failed"
	case LiveIncomplete:
		return "incomplete"
	default:
		return ""
	}
}

type LiveUsage struct {
	InputTokens       int64
	OutputTokens      int64
	InputAudioTokens  int64
	OutputAudioTokens int64
}

type LiveEvent struct {
	Kind       LiveEventKind
	Audio      []byte
	Transcript string
	Final      bool
	Outcome    LiveOutcome
	Reason     string
	Usage      LiveUsage
}

// LiveErrorCode is the deliberately content-free adapter error vocabulary.
// Raw upstream errors can include credential-bearing headers and URLs.
type LiveErrorCode uint8

const (
	LiveErrorCancelled LiveErrorCode = iota + 1
	LiveErrorUnavailable
	LiveErrorRefused
	LiveErrorMalformed
	LiveErrorTooLarge
	LiveErrorInvalid
	LiveErrorInvalidState
)

func (c LiveErrorCode) backendCode() (liveBackendErrorCode, bool) {
	switch c {
	case LiveErrorCancelled:
		return liveBackendCancelled, true
	case LiveErrorUnavailable:
		return liveBackendUnavailable, true
	case LiveErrorRefused:
		return liveBackendRefused, true
	case LiveErrorMalformed:
		return liveBackendMalformed, true
	case LiveErrorTooLarge:
		return liveBackendTooLarge, true
	case LiveErrorInvalid:
		return liveBackendInvalid, true
	case LiveErrorInvalidState:
		return liveBackendInvalidState, true
	default:
		return liveBackendUnavailable, false
	}
}

// NewLiveError returns a redacted typed error from a local vendor adapter.
func NewLiveError(code LiveErrorCode) error {
	backend, _ := code.backendCode()
	return liveError(backend)
}

// IsLiveError reports an adapter's redacted error code.
func IsLiveError(err error, code LiveErrorCode) bool {
	backend, known := code.backendCode()
	return known && liveBackendErrorIs(err, backend)
}

type localLiveBackend struct{ connection LiveConnection }

func (b localLiveBackend) append(ctx context.Context, audio []byte) error {
	return b.connection.Append(ctx, audio)
}

func (b localLiveBackend) commit(ctx context.Context) error { return b.connection.Commit(ctx) }

func (b localLiveBackend) read(ctx context.Context) (liveBackendEvent, error) {
	event, err := b.connection.Read(ctx)
	if err != nil {
		return liveBackendEvent{}, err
	}
	var kind liveBackendEventKind
	switch event.Kind {
	case LiveEventAudio:
		kind = liveBackendEventAudio
	case LiveEventTranscript:
		kind = liveBackendEventTranscript
	case LiveEventTerminal:
		kind = liveBackendEventTerminal
		if event.Outcome.word() == "" {
			return liveBackendEvent{}, liveError(liveBackendMalformed)
		}
	default:
		return liveBackendEvent{}, liveError(liveBackendMalformed)
	}
	return liveBackendEvent{
		kind: kind, audio: event.Audio, transcript: event.Transcript,
		final: event.Final, outcome: event.Outcome.word(), reason: event.Reason,
		usage: liveBackendUsage{inputTokens: event.Usage.InputTokens, outputTokens: event.Usage.OutputTokens,
			inputAudioTokens: event.Usage.InputAudioTokens, outputAudioTokens: event.Usage.OutputAudioTokens},
	}, nil
}

func (b localLiveBackend) close() { b.connection.Close() }
