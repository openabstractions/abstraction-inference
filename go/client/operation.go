package client

import (
	"context"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"math"
	"time"
)

func checkedAdmission(a Admission, err error) (Admission, error) {
	if err == nil {
		accepted := a.Outcome == wire.StartOutcomeAccepted
		err = check(accepted == (a.Operation != "") && (accepted || a.Host == "" && a.RetainedDeltas == 0))
	}
	return a, err
}

// observeOperation shares wait budgeting and cursor validation across profiles.
// Generated clients supply call; validDelta supplies the profile's payload check.
func observeOperation(ctx context.Context, transport listen.FrameClient, cursor, maxDeltas, maxBytes, waitMS int64, validDelta func(Delta) bool, call func(listen.FrameClient) (DeltaPage, error)) (DeltaPage, error) {
	if err := ctx.Err(); err != nil {
		return DeltaPage{}, err
	}
	if cursor < 0 || maxDeltas < 1 || maxDeltas > 256 || maxBytes < 1 || maxBytes > 65536 || waitMS < 0 || waitMS > 30000 {
		return DeltaPage{}, errors.New("inference client: invalid cursor or page bounds")
	}
	wait := time.Duration(waitMS) * time.Millisecond
	if transport.Timeout > time.Duration(math.MaxInt64)-wait {
		return DeltaPage{}, errors.New("inference client: wait budget overflow")
	}
	transport.Timeout += wait
	page, err := call(transport)
	if err != nil {
		return page, err
	}
	switch page.Outcome {
	case wire.PageOutcomePage:
		n := int64(len(page.Deltas))
		ok := n <= maxDeltas && cursor <= math.MaxInt64-n && page.Next == cursor+n
		for i, d := range page.Deltas {
			ok = ok && d.Sequence == cursor+int64(i) && validDelta(d)
			if d.Kind == wire.DeltaKindEnd {
				ok = ok && i == len(page.Deltas)-1 && page.AtEnd
			}
		}
		if page.AtEnd && n > 0 {
			ok = ok && page.Deltas[n-1].Kind == wire.DeltaKindEnd
		}
		err = check(ok)
	case wire.PageOutcomeGap:
		err = check(len(page.Deltas) == 0 && page.Next > cursor && !page.AtEnd)
	default:
		err = check(len(page.Deltas) == 0 && page.Next == cursor && !page.AtEnd)
	}
	return page, err
}
