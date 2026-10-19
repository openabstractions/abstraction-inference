package inference

import (
	"context"
	"errors"
	"slices"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
)

// VideoContract names abstraction.inference/video@1. A native provider that
// serves it declares router profile video (router.ProfileVideo), and the job
// executor selects such a provider for job profile video (INF-G5).
const VideoContract = "abstraction.inference/video@1"

// nativeVideoJobBackend runs profile video on a native provider through its
// video@1 service, over the authenticated transport image@1 operations use
// (image_native.go). The engine and its state live behind the provider; the
// runtime holds only the provider's operation id.
type nativeVideoJobBackend struct {
	maxInput, maxOutput int64
}

func (nativeVideoJobBackend) Profile(request jobwire.Request) string {
	if request.Profile == jobwire.ProfileVideo {
		return router.ProfileVideo
	}
	return ""
}

func (nativeVideoJobBackend) Supports(host *router.Host, request jobwire.Request) bool {
	if host == nil || host.Wire != router.WireNative || request.Profile != jobwire.ProfileVideo || !slices.Contains(host.HostProfiles(), router.ProfileVideo) {
		return false
	}
	_, ok := host.NativeTransport()
	return ok
}

// Reconcile sends the same Start again. Its request_id is the job operation,
// and a provider that still holds that request_id returns the operation it
// admitted instead of starting another (INF-G7). The provider ignores the
// frame for a request_id it holds, so a frame the runtime can no longer stage
// is named without being sent.
func (b nativeVideoJobBackend) Reconcile(ctx context.Context, submission durableJobSubmission) (durableJobStatus, error) {
	return b.start(ctx, submission, true)
}

func (b nativeVideoJobBackend) Submit(ctx context.Context, submission durableJobSubmission) (durableJobStatus, error) {
	return b.start(ctx, submission, false)
}

// start stages an edit job's first frame and sends Start. A definite failure
// is returned only before any call reaches the provider on a first
// submission; a failed or interrupted call is an error, which leaves the job
// uncertain until a provider answers.
func (b nativeVideoJobBackend) start(ctx context.Context, submission durableJobSubmission, reconciling bool) (durableJobStatus, error) {
	transport, ok := submission.host.NativeTransport()
	if !ok {
		if reconciling {
			return durableJobStatus{}, errors.New("native transport unavailable")
		}
		return durableJobStatus{State: durableJobFailed, Reason: "native:transport"}, nil
	}
	image := submission.request.Image
	forwarded := wire.VideoRequest{RequestID: submission.operation, Model: submission.model, Prompt: image.Prompt, Size: image.Size, DurationMs: submission.request.DurationMs}
	forwarded.Extensions, forwarded.RequiredExtensions = forwardedNativeExtensions(image.Extensions, image.RequiredExtensions, submission.program)
	if image.Mode == jobwire.ModeEdit {
		forwarded.ImageDigest, forwarded.ImageMediaType = image.ImageDigest, image.ImageMediaType
		input, resolved := submission.inputs[image.ImageDigest]
		if !resolved && !reconciling {
			return durableJobStatus{State: durableJobFailed, Reason: "native:input"}, nil
		}
		if resolved {
			err := mediationcontent.Upload(ctx, transport, submission.operation+"-"+image.ImageDigest[7:19], image.ImageDigest, input.mediaType, input.data, b.maxInput)
			switch {
			case err == nil:
			case !errors.Is(err, mediationcontent.ErrRefusedLocally):
				return durableJobStatus{}, err
			case !reconciling:
				return durableJobStatus{State: durableJobFailed, Reason: "native:input"}, nil
			}
		}
	}
	admission, err := wire.NewVideoClient(transport.WithContext(ctx)).Start(forwarded)
	if err != nil {
		return durableJobStatus{}, err
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		return durableJobStatus{State: durableJobFailed, Reason: nativeVideoReason(admission.Outcome.String() + ":" + admission.Reason)}, nil
	}
	if admission.Operation == "" || len(admission.Operation) > 128 {
		return durableJobStatus{}, errors.New("invalid native video admission")
	}
	return durableJobStatus{State: durableJobRunning, Handle: admission.Operation}, nil
}

// Poll reads the provider's view. An operation the provider no longer knows,
// because it or its engine restarted, fails the job as provider:lost: the
// executor never submits the work again.
func (b nativeVideoJobBackend) Poll(ctx context.Context, host *router.Host, handle string, request jobwire.Request, _ map[string]string) (durableJobStatus, error) {
	transport, ok := host.NativeTransport()
	if !ok {
		return durableJobStatus{}, errors.New("native transport unavailable")
	}
	observed, err := wire.NewVideoClient(transport.WithContext(ctx)).Observe(handle)
	if err != nil {
		return durableJobStatus{}, err
	}
	status := durableJobStatus{Handle: handle}
	switch observed.Status {
	case wire.VideoStateQueued:
		status.State = durableJobRunning
		status.Progress = durableJobProgress{Queued: true, Position: max(observed.QueuePosition, 0)}
	case wire.VideoStateGenerating:
		status.State = durableJobRunning
		if observed.Steps > 0 && observed.Step >= 0 && observed.Step <= observed.Steps {
			status.Progress = durableJobProgress{Done: observed.Step, Total: observed.Steps}
		}
	case wire.VideoStateCompleted:
		delivery := observed.Delivery
		if delivery == nil || delivery.Location != "local" || !canonicalSHA256(delivery.Digest) || delivery.Size < 1 || delivery.Size > b.maxOutput {
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: "native:delivery"}, nil
		}
		media := mediationcontent.SniffedMedia(delivery.MediaType)
		if !generatedJobMedia(request.Profile, media) {
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: "native:delivery"}, nil
		}
		data, err := mediationcontent.Download(ctx, transport, delivery.Digest, media, delivery.Size, b.maxOutput)
		if err != nil {
			if ctx.Err() != nil {
				return durableJobStatus{}, ctx.Err()
			}
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: "native:content"}, nil
		}
		status.State, status.Outputs = durableJobSucceeded, []durableJobOutput{{Data: data, MediaType: media}}
	case wire.VideoStateFailed:
		status.State, status.Reason = durableJobFailed, nativeVideoReason(observed.Reason)
	case wire.VideoStateCancelled:
		status.State = durableJobCancelled
	case wire.VideoStateUnknown:
		status.State, status.Reason = durableJobFailed, "provider:lost"
	case wire.VideoStateInvalid:
		status.State, status.Reason = durableJobFailed, "native:invalid"
	case wire.VideoStateForbidden:
		// The provider will not show this runtime the operation again.
		status.State, status.Reason = durableJobFailed, "native:forbidden"
	default:
		return durableJobStatus{}, errors.New("native video observation " + observed.Status.String())
	}
	return status, nil
}

func (nativeVideoJobBackend) Cancel(ctx context.Context, host *router.Host, handle string, _ map[string]string) error {
	transport, ok := host.NativeTransport()
	if !ok {
		return errors.New("native transport unavailable")
	}
	cancelled, err := wire.NewVideoClient(transport.WithContext(ctx)).Cancel(handle)
	if err != nil {
		return err
	}
	switch cancelled.Outcome {
	case wire.CancelOutcomeCancelled, wire.CancelOutcomeEnded, wire.CancelOutcomeUnknown:
		return nil
	}
	return errors.New("native video cancel " + cancelled.Outcome.String())
}

// nativeVideoReason is the failure word a provider's reason becomes: native:
// followed by the reason, or native:failed when it is empty or out of bounds.
func nativeVideoReason(reason string) string {
	if reason == "" || len(reason) > 256 || !utf8.ValidString(reason) {
		return "native:failed"
	}
	return "native:" + reason
}
