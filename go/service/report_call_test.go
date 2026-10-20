package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// A run of replies whose callers had already closed is reported once, and
// reporting resumes after a reply is delivered. On R28 modelhost logged
// "write pipe: The pipe is being closed." once per runtime probe that had
// timed out during one model load.
func TestReplyToAClosedCallerIsReportedOnce(t *testing.T) {
	var reported []error
	h := &Host{ctx: context.Background(), OnError: func(err error) { reported = append(reported, err) }}
	closed := fmt.Errorf("write pipe: %w", io.ErrClosedPipe)
	for range 5 {
		h.reportCall(closed)
	}
	if len(reported) != 1 || !errors.Is(reported[0], io.ErrClosedPipe) {
		t.Fatalf("five closed callers reported %v, want one report", reported)
	}
	other := errors.New("frame too large")
	h.reportCall(other)
	if len(reported) != 2 || reported[1] != other {
		t.Fatalf("another failure: reported %v", reported)
	}
	h.reportCall(closed)
	h.reportCall(nil)
	h.reportCall(closed)
	if len(reported) != 3 {
		t.Fatalf("after a delivered reply a closed caller is reported again: %d reports", len(reported))
	}
}
