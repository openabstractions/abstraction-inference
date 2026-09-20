package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
	router "github.com/openabstractions/abstraction-router/go"
)

const (
	InferenceJobKind                = "inference"
	RecoverableUpstreamGuarantee    = "abstraction.inference/recoverable-upstream@1"
	inferenceJobExecutionProfile    = "download-http-request-v1"
	defaultInferenceJobPollInterval = 5 * time.Second
)

// JobSubjectResolver recovers the exact original caller from the authenticated
// job scope and its named credential. Scope itself grants no authority.
type JobSubjectResolver func(context.Context, string, string) (Subject, ContentOutcome)

type inferenceJobWork struct {
	Scope   string          `json:"scope"`
	Request jobwire.Request `json:"request"`
}

type inferenceJobCheckpoint struct {
	Phase   string             `json:"phase"`
	Host    string             `json:"host,omitempty"`
	Binding string             `json:"binding,omitempty"`
	Model   string             `json:"model,omitempty"`
	Adapter string             `json:"adapter,omitempty"`
	Handle  string             `json:"handle,omitempty"`
	Waiting string             `json:"waiting,omitempty"`
	Failure string             `json:"failure,omitempty"`
	Result  *jobwire.JobResult `json:"result,omitempty"`
}

type durableJobState string

const (
	durableJobUnknown   durableJobState = "unknown"
	durableJobRunning   durableJobState = "running"
	durableJobSucceeded durableJobState = "succeeded"
	durableJobFailed    durableJobState = "failed"
	durableJobCancelled durableJobState = "cancelled"
)

type durableJobOutput struct {
	Data      []byte
	MediaType string
}

type durableJobStatus struct {
	State   durableJobState
	Handle  string
	Outputs []durableJobOutput
	Reason  string
}

type durableJobBackend interface {
	Supports(*router.Host, jobwire.Request) bool
	Reconcile(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error)
	Submit(context.Context, *router.Host, string, string, jobwire.Request, map[string]string) (durableJobStatus, error)
	Poll(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error)
	Cancel(context.Context, *router.Host, string, map[string]string) error
}

// JobExecution executes kind inference through the existing job leases and
// acceptance lifecycle. Its Profile deliberately retains the runtime's current
// download execution profile: adding a new kind changes no existing prepared
// download meaning, and recovery rechecks every old preparation.
type JobExecution struct {
	provider       *Provider
	resolveSubject JobSubjectResolver
	backend        durableJobBackend
	pollInterval   time.Duration
	leaseTTL       time.Duration
	onError        func(error)
}

func NewJobExecution(provider *Provider, resolve JobSubjectResolver, onError func(error)) *JobExecution {
	var client = (*http.Client)(nil)
	if provider != nil {
		client = provider.cfg.HTTP
	}
	maxBytes := int64(0)
	if provider != nil {
		maxBytes = provider.cfg.MaxImageOutputBytes
	}
	maxEach := int64(0)
	if provider != nil {
		maxEach = provider.cfg.MaxGeneratedImageBytes
	}
	return &JobExecution{provider: provider, resolveSubject: resolve, backend: replicateJobBackend{client: client, maxEach: maxEach, maxTotal: maxBytes}, pollInterval: defaultInferenceJobPollInterval, leaseTTL: 30 * time.Second, onError: onError}
}

func (*JobExecution) Profile() string { return inferenceJobExecutionProfile }

func (*JobExecution) ExecutionGuarantees() []string { return []string{RecoverableUpstreamGuarantee} }
func (*JobExecution) RecoveryGuarantees() []string  { return []string{RecoverableUpstreamGuarantee} }

func (e *JobExecution) Prepare(id, kind string, raw []byte) ([]byte, error) {
	work, _, err := e.PrepareScoped("", id, kind, raw, nil)
	return work, err
}

func (e *JobExecution) PrepareWithGuarantees(id, kind string, raw []byte, required []string) ([]byte, []string, error) {
	return e.PrepareScoped("", id, kind, raw, required)
}

func decodeInferenceJob(raw []byte) (jobwire.Request, error) {
	document, err := jobwire.Decode(raw)
	if err != nil || document.Request == nil || document.Result != nil {
		return jobwire.Request{}, errors.New("invalid inference job request")
	}
	if err := validateInferenceJob(*document.Request); err != nil {
		return jobwire.Request{}, err
	}
	return *document.Request, nil
}

func validateInferenceJob(request jobwire.Request) error {
	image := request.Image
	wireRequest := imageWireRequest(image)
	if reason := validateImageRequest(wireRequest); reason != "" {
		return fmt.Errorf("invalid image request: %s", reason)
	}
	switch request.Profile {
	case jobwire.ProfileVideo:
		if request.DurationMs < 1 || request.DurationMs > 600000 || image.Mode != jobwire.ModeGenerate || image.Count != 1 {
			return errors.New("invalid video request")
		}
	case jobwire.ProfileImageBatch:
		if request.DurationMs != 0 {
			return errors.New("invalid image batch request")
		}
	default:
		return errors.New("invalid inference job profile")
	}
	return nil
}

func (e *JobExecution) PrepareScoped(scope, id, kind string, raw []byte, required []string) ([]byte, []string, error) {
	if kind != InferenceJobKind || id == "" || scope == "" || !slices.Contains(required, RecoverableUpstreamGuarantee) {
		return nil, nil, errors.New("unsupported inference work")
	}
	request, err := decodeInferenceJob(raw)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := json.Marshal(inferenceJobWork{Scope: scope, Request: request})
	if err != nil {
		return nil, nil, err
	}
	return prepared, []string{RecoverableUpstreamGuarantee}, nil
}

func imageModeFromJob(mode jobwire.Mode) wire.ImageMode {
	converted, _ := wire.ParseImageMode(mode.String())
	return converted
}

func requestGuaranteesFromJob(values []jobwire.RequestGuarantee) []wire.RequestGuarantee {
	converted := make([]wire.RequestGuarantee, len(values))
	for i, value := range values {
		converted[i] = wire.RequestGuarantee(value)
	}
	return converted
}

func imageWireRequest(in jobwire.ImageRequest) wire.ImageRequest {
	return wire.ImageRequest{Model: in.Model, Mode: imageModeFromJob(in.Mode), Prompt: in.Prompt, Size: in.Size, Count: in.Count,
		ImageDigest: in.ImageDigest, ImageMediaType: in.ImageMediaType, MaskDigest: in.MaskDigest, MaskMediaType: in.MaskMediaType,
		Guarantees: requestGuaranteesFromJob(in.Guarantees), Credential: in.Credential, RequiredExtensions: slices.Clone(in.RequiredExtensions), Extensions: mapsClone(in.Extensions)}
}

func mapsClone(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type inferenceJobPlan struct {
	subject Subject
	host    *router.Host
	model   string
	adapter string
	headers map[string]string
	commits []ContentCommitter
}

func (e *JobExecution) plan(ctx context.Context, work inferenceJobWork, pin inferenceJobCheckpoint, operationID string) (inferenceJobPlan, acceptance.AcceptanceOutcome, string) {
	if e.provider == nil || e.resolveSubject == nil || e.backend == nil {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "provider:unavailable"
	}
	subject, outcome := e.resolveSubject(ctx, work.Scope, work.Request.Image.Credential)
	switch outcome {
	case ContentResolved:
	case ContentForbidden, ContentUnknown:
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "caller:forbidden"
	default:
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "caller:unavailable"
	}
	request := imageWireRequest(work.Request.Image)
	var host *router.Host
	model := pin.Model
	if pin.Phase == "" {
		var reason string
		host, model, reason = e.provider.pick(ctx, request.Model, requestGuaranteeWords(request.Guarantees), request.Credential, router.ProfileImage)
		if host == nil {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, "host:" + reason
		}
	} else {
		host = e.pinnedHost(pin.Host)
		if host == nil || !host.Servable() || model == "" || host.Credential != request.Credential {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "host:unavailable"
		}
		// A legacy marker has no evidence of the original binding. Preserve its
		// uncertainty instead of adopting today's same-name configuration.
		if pin.Binding == "" {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "host:unbound"
		}
		if pin.Binding != inferenceHostBinding(host) || pin.Adapter != imageAdapterFor(host) {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "host:replaced"
		}
	}
	adapter := imageAdapterFor(host)
	if reason := unsupportedImage(adapter, host, request); reason != "" || !e.backend.Supports(host, work.Request) {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, "host:unsupported"
	}
	word, err := e.provider.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "rights:unavailable"
	}
	if word != "permitted" {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "rights:" + word
	}
	if request.Mode == wire.ImageModeEdit {
		if _, resolved, why := e.provider.resolveImageInput(ctx, subject, request.ImageDigest, request.ImageMediaType); why != "" {
			if resolved == wire.StartOutcomeForbidden {
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, why
			}
			if resolved == wire.StartOutcomeUnavailable {
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, why
			}
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, why
		}
		if request.MaskDigest != "" {
			if _, resolved, why := e.provider.resolveImageInput(ctx, subject, request.MaskDigest, request.MaskMediaType); why != "" {
				if resolved == wire.StartOutcomeForbidden {
					return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, why
				}
				if resolved == wire.StartOutcomeUnavailable {
					return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, why
				}
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, why
			}
		}
	}
	if e.provider.cfg.PrepareContentWrite == nil {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "output:unavailable"
	}
	media := "image/*"
	if work.Request.Profile == jobwire.ProfileVideo {
		media = "video/*"
	}
	maxOutput := e.provider.cfg.MaxGeneratedImageBytes
	if work.Request.Profile == jobwire.ProfileVideo {
		maxOutput = e.provider.cfg.MaxImageOutputBytes
	}
	commits := make([]ContentCommitter, 0, request.Count)
	for i := int64(0); i < request.Count; i++ {
		commit, contentOutcome := e.provider.cfg.PrepareContentWrite(ctx, subject, "image", media, maxOutput)
		if contentOutcome == ContentForbidden {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "output:forbidden"
		}
		if contentOutcome != ContentResolved || commit == nil {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "output:unavailable"
		}
		commits = append(commits, commit)
	}
	credential := host.Credential
	if credential != "" && e.provider.cfg.Ceilings != nil {
		continuing := operationID != "" && e.provider.cfg.Ceilings.DurableAttemptCounted(operationID)
		if unit, over := e.provider.cfg.Ceilings.Exceeded(credential); over && !continuing {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, "ceiling:" + unit + ":" + credential
		}
	}
	var headers map[string]string
	if credential != "" {
		headers, word = e.provider.cfg.Apply(ctx, subject, ImageContract, credential, router.Target(host.Base))
		if word != "applied" {
			if word == "unavailable" || word == "" {
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "credential:unavailable"
			}
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "credential:" + word
		}
	}
	return inferenceJobPlan{subject: subject, host: host, model: model, adapter: adapter, headers: headers, commits: commits}, acceptance.AcceptanceOutcomeAccepted, ""
}

func (e *JobExecution) CheckAdmission(scope, kind string, raw []byte, required []string) (acceptance.AcceptanceOutcome, string) {
	if kind != InferenceJobKind || !slices.Contains(required, RecoverableUpstreamGuarantee) {
		return acceptance.AcceptanceOutcomeInvalid, "recoverable-upstream guarantee required"
	}
	request, err := decodeInferenceJob(raw)
	if err != nil {
		return acceptance.AcceptanceOutcomeInvalid, "invalid request"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, outcome, reason := e.plan(ctx, inferenceJobWork{Scope: scope, Request: request}, inferenceJobCheckpoint{}, "")
	return outcome, reason
}

func (e *JobExecution) Serve(ctx context.Context, store job.Store) error {
	watch := job.Watch(store, InferenceJobKind)
	defer watch.Close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		e.sweep(ctx, store)
		select {
		case <-ctx.Done():
			return nil
		case <-watch.Changes():
		case <-ticker.C:
		}
	}
	return nil
}

func (e *JobExecution) sweep(ctx context.Context, store job.Store) {
	if e.provider.cfg.Ceilings != nil {
		if all, listErr := store.List(); listErr == nil {
			active := map[string]bool{}
			for _, record := range all {
				if record.Kind == InferenceJobKind && !record.State.Terminal() {
					active[record.ID] = true
				}
			}
			if err := e.provider.cfg.Ceilings.PruneDurable(active); err != nil && e.onError != nil {
				e.onError(fmt.Errorf("inference job accounting prune: %w", err))
			}
		}
	}
	records, err := store.Orphans()
	if err != nil && e.onError != nil {
		e.onError(fmt.Errorf("inference job sweep: %w", err))
	}
	for _, record := range records {
		if ctx.Err() != nil || record.Kind != InferenceJobKind || !store.Claimable(record) {
			continue
		}
		held, claimErr := store.Claim(record.ID, "inference-service-"+job.NewID(), 30*time.Second)
		if claimErr != nil {
			continue
		}
		if err := e.execute(ctx, store, held); err != nil && e.onError != nil {
			e.onError(fmt.Errorf("inference job %s: %w", record.ID, err))
		}
	}
}

func decodeInferenceWork(record *job.Record) (inferenceJobWork, error) {
	var work inferenceJobWork
	if record.Kind != InferenceJobKind || record.DecodeSpec(&work) != nil || work.Scope == "" {
		return inferenceJobWork{}, errors.New("invalid prepared inference work")
	}
	if err := validateInferenceJob(work.Request); err != nil {
		return inferenceJobWork{}, err
	}
	return work, nil
}

func (e *JobExecution) execute(ctx context.Context, store job.Store, held *job.Record) error {
	ctx, lease := holdInferenceLease(ctx, store, held, e.leaseTTL)
	defer lease.stop()
	work, err := decodeInferenceWork(held)
	if err != nil {
		lease.stop()
		return e.finish(store, held, job.StateFailed, inferenceJobCheckpoint{Failure: "invalid"})
	}
	var checkpoint inferenceJobCheckpoint
	if err := held.DecodeCheckpoint(&checkpoint); err != nil {
		lease.stop()
		return e.finish(store, held, job.StateFailed, inferenceJobCheckpoint{Failure: "checkpoint"})
	}
	if held.Wants() == job.WantCancel && checkpoint.Phase == "" {
		lease.stop()
		if err := e.finish(store, held, job.StateCancelled, checkpoint); err != nil {
			return err
		}
		e.recordJobAttempt(ctx, held, work, inferenceJobPlan{}, "cancelled", "", 0)
		return nil
	}
	plan, outcome, reason := e.plan(ctx, work, checkpoint, held.ID)
	if outcome != acceptance.AcceptanceOutcomeAccepted {
		if outcome != acceptance.AcceptanceOutcomeUnavailable {
			checkpoint.Failure = reason
			lease.stop()
			if err := e.finish(store, held, job.StateFailed, checkpoint); err != nil {
				return err
			}
			e.recordJobAttempt(ctx, held, work, plan, "failed", reason, 0)
			return nil
		}
		return e.releaseWaitingRecorded(ctx, store, held, checkpoint, reason, work, plan)
	}
	defer clear(plan.headers)
	freshSubmission := checkpoint.Phase == ""
	host := plan.host
	if e.provider.cfg.Ceilings != nil && host.Credential != "" {
		if err := e.provider.cfg.Ceilings.AddDurableAttempt(held.ID, host.Credential); err != nil {
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "ceiling:unavailable", work, plan)
		}
	}
	if freshSubmission {
		checkpoint = inferenceJobCheckpoint{Phase: "submitting", Host: plan.host.Name, Binding: inferenceHostBinding(plan.host), Model: plan.model, Adapter: plan.adapter}
		held, err = store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error {
			record.State, record.Error = job.StateRunning, ""
			return record.SetCheckpoint(checkpoint)
		})
		if err != nil {
			return err
		}
		if err := lease.renew(); err != nil {
			return err
		}
	}
	var status durableJobStatus
	if checkpoint.Handle == "" {
		if freshSubmission {
			status, err = e.backend.Submit(ctx, host, held.ID, checkpoint.Model, work.Request, plan.headers)
		} else {
			// A persisted submitting marker means the process may have reached
			// the paid upstream before it died. Only provider reconciliation may
			// recover it; replaying Submit can charge and produce twice.
			status, err = e.backend.Reconcile(ctx, host, held.ID, work.Request, plan.headers)
		}
		if err != nil || status.Handle == "" {
			checkpoint.Waiting = "upstream:uncertain"
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, checkpoint.Waiting, work, plan)
		}
		checkpoint.Phase, checkpoint.Handle, checkpoint.Waiting = "submitted", status.Handle, ""
		held, err = store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error { return record.SetCheckpoint(checkpoint) })
		if err != nil {
			return err
		}
	}
	for {
		current, loadErr := store.Load(held.ID)
		if loadErr != nil {
			return loadErr
		}
		if current.Wants() == job.WantCancel {
			if cancelErr := e.backend.Cancel(ctx, host, checkpoint.Handle, plan.headers); cancelErr != nil {
				return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "cancel:unavailable", work, plan)
			}
			return e.finishRecorded(store, held, job.StateCancelled, checkpoint, work, plan, 0, lease)
		}
		if status.State == "" || status.State == durableJobRunning {
			status, err = e.backend.Poll(ctx, host, checkpoint.Handle, work.Request, plan.headers)
			if err != nil {
				return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "upstream:unavailable", work, plan)
			}
		}
		switch status.State {
		case durableJobSucceeded:
			return e.publish(ctx, store, held, work, plan, checkpoint, status.Outputs, lease)
		case durableJobFailed:
			checkpoint.Failure = status.Reason
			return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, 0, lease)
		case durableJobCancelled:
			return e.finishRecorded(store, held, job.StateCancelled, checkpoint, work, plan, 0, lease)
		case durableJobUnknown:
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "upstream:uncertain", work, plan)
		}
		select {
		case <-ctx.Done():
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "service:stopped", work, plan)
		case <-time.After(e.pollInterval):
		}
	}
}

// inferenceHostBinding pins configuration, never secrets. It does not claim
// that an unchanged URL proves an upstream's continuity; a registered provider
// additionally supplies its service-owned BindingID.
func inferenceHostBinding(host *router.Host) string {
	data, _ := json.Marshal(struct {
		Name, Registration, Base, Path, Wire, Credential, DeclaredBy, Domain string
		Hosted                                                               bool
	}{host.Name, host.BindingID, host.Base, host.Chat, host.Wire, host.Credential, host.DeclaredBy, host.Domain, host.Hosted})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (e *JobExecution) pinnedHost(name string) *router.Host {
	for _, host := range e.provider.cfg.Router.Hosts() {
		if host.Name == name {
			return host
		}
	}
	return nil
}

func (e *JobExecution) publish(ctx context.Context, store job.Store, held *job.Record, work inferenceJobWork, plan inferenceJobPlan, checkpoint inferenceJobCheckpoint, outputs []durableJobOutput, lease *inferenceLease) error {
	if e.provider.cfg.Ceilings != nil && plan.host.Credential != "" {
		if err := e.provider.cfg.Ceilings.AddDurableImages(held.ID, plan.host.Credential, int64(len(outputs))); err != nil {
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "ceiling:unavailable", work, plan)
		}
	}
	if len(outputs) != len(plan.commits) {
		checkpoint.Failure = "upstream:count"
		return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, 0, lease)
	}
	deliveries := make([]jobwire.Delivery, 0, len(outputs))
	var total int64
	for _, output := range outputs {
		size := int64(len(output.Data))
		perOutput := e.provider.cfg.MaxGeneratedImageBytes
		if work.Request.Profile == jobwire.ProfileVideo {
			perOutput = e.provider.cfg.MaxImageOutputBytes
		}
		if size == 0 || (!generatedInputMedia(output.MediaType) && output.MediaType != "video/mp4") || size > perOutput || size > e.provider.cfg.MaxImageOutputBytes-total || http.DetectContentType(output.Data) != output.MediaType {
			checkpoint.Failure = "upstream:output"
			return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, 0, lease)
		}
		total += size
	}
	for i, output := range outputs {
		size := int64(len(output.Data))
		sum := sha256.Sum256(output.Data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if err := lease.renew(); err != nil || ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		if outcome := plan.commits[i](ctx, digest, output.Data); outcome != ContentResolved {
			checkpoint.Failure = "output:" + string(outcome)
			return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, int64(len(deliveries)), lease)
		}
		if err := lease.renew(); err != nil {
			return err
		}
		deliveries = append(deliveries, jobwire.Delivery{Digest: digest, MediaType: output.MediaType, Size: size, Location: "local"})
	}
	result := jobwire.JobResult{Profile: work.Request.Profile, Deliveries: deliveries, Host: checkpoint.Host, Model: checkpoint.Model}
	checkpoint.Phase, checkpoint.Result, checkpoint.Waiting = "complete", &result, ""
	return e.finishRecorded(store, held, job.StateComplete, checkpoint, work, plan, int64(len(outputs)), lease)
}

func (e *JobExecution) releaseWaiting(store job.Store, held *job.Record, checkpoint inferenceJobCheckpoint, waiting string) error {
	checkpoint.Waiting = waiting
	updated, err := store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error {
		record.State, record.Error = job.StateRunning, waiting
		return record.SetCheckpoint(checkpoint)
	})
	if err != nil {
		return err
	}
	return store.Release(updated.ID, updated.Lease.Epoch)
}

func (e *JobExecution) releaseWaitingRecorded(ctx context.Context, store job.Store, held *job.Record, checkpoint inferenceJobCheckpoint, waiting string, work inferenceJobWork, plan inferenceJobPlan) error {
	if err := e.releaseWaiting(store, held, checkpoint, waiting); err != nil {
		return err
	}
	e.recordJobAttempt(ctx, held, work, plan, "waiting", waiting, 0)
	return nil
}

func (e *JobExecution) finish(store job.Store, held *job.Record, state job.State, checkpoint inferenceJobCheckpoint) error {
	_, err := store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error {
		record.State = state
		record.Error = checkpoint.Failure
		if state == job.StateComplete {
			record.Progress.Done, record.Progress.Total = 1, 1
		}
		return record.SetCheckpoint(checkpoint)
	})
	return err
}

func (e *JobExecution) finishRecorded(store job.Store, held *job.Record, state job.State, checkpoint inferenceJobCheckpoint, work inferenceJobWork, plan inferenceJobPlan, images int64, lease *inferenceLease) error {
	lease.stop()
	if err := e.finish(store, held, state, checkpoint); err != nil {
		return err
	}
	outcome := "failed"
	if state == job.StateComplete {
		outcome = "completed"
	} else if state == job.StateCancelled {
		outcome = "cancelled"
	}
	e.recordJobAttempt(context.Background(), held, work, plan, outcome, checkpoint.Failure, images)
	return nil
}

func (e *JobExecution) recordJobAttempt(ctx context.Context, held *job.Record, work inferenceJobWork, plan inferenceJobPlan, outcome, reason string, images int64) {
	subject := plan.subject
	if subject.Account == "" {
		subject, _ = e.resolveSubject(ctx, work.Scope, work.Request.Image.Credential)
	}
	host, model, credential := "", work.Request.Image.Model, work.Request.Image.Credential
	if plan.host != nil {
		host, model, credential = plan.host.Name, plan.model, plan.host.Credential
	}
	record := Record{Operation: held.ID, Profile: router.ProfileImage, Route: RouteNative, Account: subject.Account, Program: subject.Program,
		Host: host, Model: model, Family: familyOf(model), Credential: credential, Outcome: outcome, Reason: reason, Images: images}
	if e.provider.cfg.Ceilings != nil && credential != "" {
		record.Ceiling = e.provider.cfg.Ceilings.State(credential)
	}
	e.provider.record(record)
}

func (e *JobExecution) OperationWaiting(record *job.Record) string {
	if record.Kind != InferenceJobKind {
		return ""
	}
	var checkpoint inferenceJobCheckpoint
	if record.DecodeCheckpoint(&checkpoint) == nil {
		return checkpoint.Waiting
	}
	return ""
}

func (e *JobExecution) OperationFailure(record *job.Record) *acceptance.WorkFailure {
	if record.Kind != InferenceJobKind || record.State != job.StateFailed {
		return nil
	}
	var checkpoint inferenceJobCheckpoint
	_ = record.DecodeCheckpoint(&checkpoint)
	message := "inference work failed"
	if checkpoint.Failure != "" {
		message = checkpoint.Failure
	}
	return &acceptance.WorkFailure{Classification: acceptance.FailureClassPermanent, Message: message, Cause: acceptance.FailureCauseOther}
}

func (*JobExecution) ResultRetentionMs() int64 { return acceptanceprovider.MinimumRetentionMs }

func (e *JobExecution) ReadOperationResult(_ string, record *job.Record, offset, maxBytes int64) ([]byte, int64, error) {
	if record.Kind != InferenceJobKind || record.State != job.StateComplete {
		return nil, 0, acceptanceprovider.ErrResultLost
	}
	var checkpoint inferenceJobCheckpoint
	if record.DecodeCheckpoint(&checkpoint) != nil || checkpoint.Result == nil {
		return nil, 0, acceptanceprovider.ErrResultLost
	}
	data := jobwire.Encode(&jobwire.Document{Result: checkpoint.Result})
	total := int64(len(data))
	if offset < 0 || offset > total || maxBytes < 1 {
		return nil, total, acceptanceprovider.ErrResultRange
	}
	end := total
	if maxBytes < total-offset {
		end = offset + maxBytes
	}
	return bytes.Clone(data[offset:end]), total, nil
}
