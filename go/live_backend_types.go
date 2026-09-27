package inference

import (
	"context"
	"errors"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

const (
	maxLiveAudioBytes      = 64 << 10
	maxLiveTranscriptBytes = 16 << 10
)

type liveBackend interface {
	append(context.Context, []byte) error
	commit(context.Context) error
	read(context.Context) (liveBackendEvent, error)
	close()
}

type liveBackendEventKind string

const (
	liveBackendEventAudio      liveBackendEventKind = "audio"
	liveBackendEventTranscript liveBackendEventKind = "transcript"
	liveBackendEventTerminal   liveBackendEventKind = "terminal"
)

type liveBackendUsage struct {
	inputTokens       int64
	outputTokens      int64
	inputAudioTokens  int64
	outputAudioTokens int64
}

type liveBackendEvent struct {
	kind       liveBackendEventKind
	audio      []byte
	transcript string
	final      bool
	outcome    string
	reason     string
	usage      liveBackendUsage
	remoteEnd  *wire.LiveReply
}

type liveBackendErrorCode string

const (
	liveBackendCancelled    liveBackendErrorCode = "cancelled"
	liveBackendUnavailable  liveBackendErrorCode = "unavailable"
	liveBackendRefused      liveBackendErrorCode = "refused"
	liveBackendMalformed    liveBackendErrorCode = "malformed"
	liveBackendTooLarge     liveBackendErrorCode = "too_large"
	liveBackendInvalid      liveBackendErrorCode = "invalid"
	liveBackendInvalidState liveBackendErrorCode = "invalid_state"
)

type liveBackendError struct{ code liveBackendErrorCode }

func (e *liveBackendError) Error() string { return "live backend: " + string(e.code) }

func liveError(code liveBackendErrorCode) error { return &liveBackendError{code: code} }

func liveErrorForContext(ctx context.Context, fallback liveBackendErrorCode) error {
	if ctx.Err() != nil {
		return liveError(liveBackendCancelled)
	}
	return liveError(fallback)
}

func liveBackendErrorIs(err error, code liveBackendErrorCode) bool {
	var target *liveBackendError
	return errors.As(err, &target) && target.code == code
}
