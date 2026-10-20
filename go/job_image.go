package inference

import (
	"context"
	"errors"
	"slices"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
)

// nativeImageJobBackend runs profile image_batch on a native image provider
// through its image@1 service, over the authenticated transport a
// synchronous image@1 call uses (runNativeImage). The provider holds the
// operation and its deltas; the runtime holds only the operation id. Each
// poll reads the retained deltas from the start, so a poll after a runtime
// restart sees what the first one would have. The executor publishes the
// copied images through the job's prepared output commits (INF-S7).
type nativeImageJobBackend struct {
	maxInput, maxEach, maxTotal int64
}

func (nativeImageJobBackend) Profile(request jobwire.Request) string {
	if request.Profile == jobwire.ProfileImageBatch {
		return router.ProfileImage
	}
	return ""
}

func (nativeImageJobBackend) Supports(host *router.Host, request jobwire.Request) bool {
	if host == nil || host.Wire != router.WireNative || request.Profile != jobwire.ProfileImageBatch || !slices.Contains(host.HostProfiles(), router.ProfileImage) {
		return false
	}
	_, ok := host.NativeTransport()
	return ok
}

// Reconcile sends the same Start again. A provider that admitted the lost
// Start answers with that operation, matched by caller and job mark (INF-G5);
// one that never saw it starts the work, as a job planned afresh does
// (INF-G10). Work on a hosted native server stays uncertain and is
// never sent again, since a second Start could be charged twice.
func (b nativeImageJobBackend) Reconcile(ctx context.Context, submission durableJobSubmission) (durableJobStatus, error) {
	if submission.host.Hosted {
		return durableJobStatus{State: durableJobUnknown}, nil
	}
	return b.start(ctx, submission, true)
}

func (b nativeImageJobBackend) Submit(ctx context.Context, submission durableJobSubmission) (durableJobStatus, error) {
	return b.start(ctx, submission, false)
}

// jobImageExtensions are the controls a job's image@1 Start forwards: the
// caller's own, the bound app's claim, and the job mark naming the job's
// operation. The job, not an Observe loop, owns the provider's operation, and
// the provider waits out a runtime restart for it (INF-G5).
func jobImageExtensions(image jobwire.ImageRequest, submission durableJobSubmission) (map[string]string, []string) {
	values, required := forwardedNativeExtensions(image.Extensions, image.RequiredExtensions, submission.program)
	values[JobExtension] = submission.operation
	return values, required
}

// start stages an edit's image and mask and sends Start with the job's
// prompt, size and count, as runNativeImage forwards a synchronous call. A
// definite failure is returned only before any call reaches the provider on
// a first submission; a failed or interrupted call is an error, which leaves
// the job uncertain.
func (b nativeImageJobBackend) start(ctx context.Context, submission durableJobSubmission, reconciling bool) (durableJobStatus, error) {
	transport, ok := submission.host.NativeTransport()
	if !ok {
		if reconciling {
			return durableJobStatus{}, errors.New("native transport unavailable")
		}
		return durableJobStatus{State: durableJobFailed, Reason: "native:transport"}, nil
	}
	image := submission.request.Image
	forwarded := imageWireRequest(image)
	forwarded.Model, forwarded.Credential = submission.model, ""
	forwarded.Guarantees = localGuarantees(forwarded.Guarantees)
	forwarded.Extensions, forwarded.RequiredExtensions = jobImageExtensions(image, submission)
	if image.Mode == jobwire.ModeEdit {
		if reason, err := stageNativeInputs(ctx, transport, submission, b.maxInput, reconciling, image.ImageDigest, image.MaskDigest); err != nil || reason != "" {
			return durableJobStatus{State: durableJobFailed, Reason: reason}, err
		}
	}
	admission, err := wire.NewImageClient(transport.WithContext(ctx)).Start(forwarded)
	if err != nil {
		return durableJobStatus{}, err
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		return durableJobStatus{State: durableJobFailed, Reason: nativeVideoReason(admission.Outcome.String() + ":" + admission.Reason)}, nil
	}
	if admission.Operation == "" || len(admission.Operation) > 128 {
		return durableJobStatus{}, errors.New("invalid native image admission")
	}
	return durableJobStatus{State: durableJobRunning, Handle: admission.Operation}, nil
}

// Poll reads the operation's deltas from the start: the latest progress
// becomes the job's progress as percent of 100, and the end delta ends the
// job. An operation the provider no longer knows, because it or its engine
// restarted, reads as provider:lost: the executor sends the work again from
// the start once, and a second loss fails the job (INF-G7).
func (b nativeImageJobBackend) Poll(ctx context.Context, host *router.Host, handle string, request jobwire.Request, _ map[string]string) (durableJobStatus, error) {
	transport, ok := host.NativeTransport()
	if !ok {
		return durableJobStatus{}, errors.New("native transport unavailable")
	}
	client := wire.NewImageClient(transport.WithContext(ctx))
	status := durableJobStatus{State: durableJobRunning, Handle: handle}
	failed := func(reason string) (durableJobStatus, error) {
		cancelAcceptedNativeImage(transport, handle)
		return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: reason}, nil
	}
	for cursor := int64(0); ; {
		page, err := client.Observe(handle, cursor, 256, 65536, 0)
		if err != nil {
			return durableJobStatus{}, err
		}
		switch page.Outcome {
		case wire.PageOutcomePage:
		case wire.PageOutcomeGap:
			// Deltas older than the provider's window are gone; the end
			// delta, which names every delivery, is still ahead.
			if page.Next <= cursor {
				return durableJobStatus{}, errors.New("native image observation gap")
			}
			cursor = page.Next
			continue
		case wire.PageOutcomeUnknown:
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: providerLost}, nil
		case wire.PageOutcomeForbidden:
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: "native:forbidden"}, nil
		case wire.PageOutcomeInvalid:
			return failed("native:invalid")
		default:
			return durableJobStatus{}, errors.New("native image observation " + page.Outcome.String())
		}
		if validateRemoteImagePage(page, cursor) != "" {
			return failed("native:invalid_page")
		}
		for _, delta := range page.Deltas {
			if remoteImageDeltaShape(delta) != "" {
				return failed("native:invalid_page")
			}
			switch delta.Kind {
			case wire.DeltaKindImageProgress:
				if percent := delta.ImageProgress.Percent; percent > 0 {
					status.Progress = durableJobProgress{Done: percent, Total: 100}
				}
			case wire.DeltaKindEnd:
				return b.ended(ctx, transport, handle, request, *delta.ImageEnd)
			}
		}
		if page.AtEnd {
			return failed("native:terminal")
		}
		if len(page.Deltas) == 0 {
			return status, nil
		}
		cursor = page.Next
	}
}

// ended is the job's status for the provider's terminal reply. A completed
// reply's deliveries are copied from the provider's content reader under
// the same bounds a synchronous call applies.
func (b nativeImageJobBackend) ended(ctx context.Context, transport listen.FrameClient, handle string, request jobwire.Request, end wire.ImageReply) (durableJobStatus, error) {
	results := make([]wire.ImageResult, len(end.Deliveries))
	for i, delivery := range end.Deliveries {
		results[i] = wire.ImageResult{Index: int64(i), Delivery: delivery}
	}
	if !validImageAccounting(end.Reason, end.Usage, end.Cost) || validateRemoteImageTerminal(imageWireRequest(request.Image), results, end) != "" {
		return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: "native:terminal"}, nil
	}
	switch end.Outcome {
	case wire.ReplyOutcomeCompleted:
		images, reason := downloadNativeImages(ctx, transport, end.Deliveries, b.maxEach, b.maxTotal)
		if reason != "" {
			if ctx.Err() != nil {
				return durableJobStatus{}, ctx.Err()
			}
			return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: reason}, nil
		}
		outputs := make([]durableJobOutput, len(images))
		for i, image := range images {
			outputs[i] = durableJobOutput{Data: image.data, MediaType: image.mediaType}
		}
		return durableJobStatus{State: durableJobSucceeded, Handle: handle, Outputs: outputs}, nil
	case wire.ReplyOutcomeCancelled:
		return durableJobStatus{State: durableJobCancelled, Handle: handle}, nil
	}
	return durableJobStatus{State: durableJobFailed, Handle: handle, Reason: nativeVideoReason(end.Outcome.String() + ":" + end.Reason)}, nil
}

func (nativeImageJobBackend) Cancel(ctx context.Context, host *router.Host, handle string, _ map[string]string) error {
	transport, ok := host.NativeTransport()
	if !ok {
		return errors.New("native transport unavailable")
	}
	return nativeCancelled(wire.NewImageClient(transport.WithContext(ctx)).Cancel(handle))
}

// nativeCancelled reads a provider's answer to Cancel: the operation was
// cancelled, had already ended, or is unknown to the provider, and in each
// case nothing more runs.
func nativeCancelled(cancelled wire.Cancellation, err error) error {
	if err != nil {
		return err
	}
	switch cancelled.Outcome {
	case wire.CancelOutcomeCancelled, wire.CancelOutcomeEnded, wire.CancelOutcomeUnknown:
		return nil
	}
	return errors.New("native cancel " + cancelled.Outcome.String())
}

// stageNativeInputs copies a job's authorized inputs named by digests into
// the provider's content store before Start (INF-G5). On a first
// submission an input the plan did not resolve, or one refused before it
// left the runtime, is a definite refusal reason: nothing reached the
// provider's Start. When reconciling, such an input is named unstaged, since
// a provider that holds the work ignores it. A transfer that may have
// reached the provider is an error.
func stageNativeInputs(ctx context.Context, transport listen.FrameClient, submission durableJobSubmission, maxInput int64, reconciling bool, digests ...string) (string, error) {
	for _, digest := range namedInputs(digests...) {
		input, resolved := submission.inputs[digest]
		if !resolved {
			if !reconciling {
				return "native:input", nil
			}
			continue
		}
		err := mediationcontent.Upload(ctx, transport, submission.operation+"-"+digest[7:19], digest, input.mediaType, input.data, maxInput)
		switch {
		case err == nil:
		case !errors.Is(err, mediationcontent.ErrRefusedLocally):
			return "", err
		case !reconciling:
			return "native:input", nil
		}
	}
	return "", nil
}

// downloadNativeImages copies a native provider's local image deliveries,
// each at most maxEach bytes and together at most maxTotal. reason is
// native:delivery for a delivery past the bound and native:content for one
// that could not be read.
func downloadNativeImages(ctx context.Context, transport listen.FrameClient, deliveries []wire.Delivery, maxEach, maxTotal int64) ([]generatedImage, string) {
	outputs := make([]generatedImage, 0, len(deliveries))
	var total int64
	for _, delivery := range deliveries {
		if delivery.Size > maxTotal-total {
			return nil, "native:delivery"
		}
		data, err := mediationcontent.Download(ctx, transport, delivery.Digest, delivery.MediaType, delivery.Size, maxEach)
		if err != nil {
			return nil, "native:content"
		}
		outputs = append(outputs, generatedImage{data: data, mediaType: delivery.MediaType})
		total += delivery.Size
	}
	return outputs, ""
}
