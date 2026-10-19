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
	"strconv"
	"strings"
	"sync"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
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
	// generatingInferenceJobPollInterval is the poll while an upstream
	// reports steps under way (pollWait).
	generatingInferenceJobPollInterval = time.Second
)

// JobSubjectResolver recovers the exact original caller from the authenticated
// job scope and its named credential. Scope itself grants no authority.
type JobSubjectResolver func(context.Context, string, string) (Subject, ContentOutcome)

// OperationJobSubjectResolver uses private job evidence for the claimed record.
// The legacy resolver remains for journals accepted before subject retention.
type OperationJobSubjectResolver func(context.Context, string, string, string) (Subject, ContentOutcome)

type inferenceJobWork struct {
	Scope   string          `json:"scope"`
	Request jobwire.Request `json:"request"`
}

type inferenceJobCheckpoint struct {
	Phase   string `json:"phase"`
	Host    string `json:"host,omitempty"`
	Binding string `json:"binding,omitempty"`
	Model   string `json:"model,omitempty"`
	Adapter string `json:"adapter,omitempty"`
	Handle  string `json:"handle,omitempty"`
	// SentMs is when the first submission began, in Unix milliseconds; it
	// bounds how long a cancel waits for an upstream answer.
	SentMs int64 `json:"sent_ms,omitempty"`
	// HostLostMs is when the executor first found the recorded server gone,
	// in Unix milliseconds; it bounds the wait for it (INF-G10).
	HostLostMs int64 `json:"host_lost_ms,omitempty"`
	// Replanned records that the job was planned afresh once because its
	// recorded server was replaced or gone; it is never planned afresh again
	// (INF-G10).
	Replanned bool               `json:"replanned,omitempty"`
	Waiting   string             `json:"waiting,omitempty"`
	Failure   string             `json:"failure,omitempty"`
	Result    *jobwire.JobResult `json:"result,omitempty"`
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
	// Progress is what a running upstream reported; the zero value reports
	// nothing.
	Progress durableJobProgress
}

// durableJobProgress is a running upstream's report: step done of total
// while it generates, or its place in the engine's queue, 1 for next, when
// Queued. The executor carries it into the job snapshot (INF-G6).
type durableJobProgress struct {
	Done, Total int64
	Queued      bool
	Position    int64
}

// waiting is the snapshot's waiting word for this report: queue:<n> while
// queued at a known position, queued while queued at an unknown one, and
// empty otherwise.
func (p durableJobProgress) waiting() string {
	switch {
	case p.Queued && p.Position > 0:
		return "queue:" + strconv.FormatInt(p.Position, 10)
	case p.Queued:
		return "queued"
	}
	return ""
}

// durableJobSubmission is everything one first submission sends: the pinned
// host and model, the request, the credential headers, the original caller's
// program a native provider records as the claim, and the authorized inputs
// the plan resolved for an edit.
type durableJobSubmission struct {
	host      *router.Host
	operation string
	model     string
	request   jobwire.Request
	headers   map[string]string
	program   string
	inputs    map[string]resolvedImageInput
}

// durableJobBackend is one way the executor reaches an upstream. Profile is
// the router profile it selects a host under for request, or "" when it never
// serves request; Supports judges the selected host. A Submit that returns no
// handle and state failed is a definite refusal: nothing runs upstream.
// Reconcile receives the submission an uncertain Submit sent, for a backend
// whose upstream recognises a repeated submission as the same work.
type durableJobBackend interface {
	Profile(jobwire.Request) string
	Supports(*router.Host, jobwire.Request) bool
	Reconcile(context.Context, durableJobSubmission) (durableJobStatus, error)
	Submit(context.Context, durableJobSubmission) (durableJobStatus, error)
	Poll(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error)
	Cancel(context.Context, *router.Host, string, map[string]string) error
}

// JobExecution executes kind inference through the existing job leases and
// acceptance lifecycle. Its Profile deliberately retains the runtime's current
// download execution profile: adding a new kind changes no existing prepared
// download meaning, and recovery rechecks every old preparation.
type JobExecution struct {
	provider                *Provider
	resolveSubject          JobSubjectResolver
	resolveOperationSubject OperationJobSubjectResolver
	// backends are tried in order: the first whose profile selects a host
	// serves the job, and a recovered job returns to the backend that
	// supports its pinned host.
	backends     []durableJobBackend
	pollInterval time.Duration
	leaseTTL     time.Duration
	onError      func(error)
	peersMu      sync.Mutex
	peers        map[string]submitterPeer
	// resends paces the reconciliation of work left uncertain (INF-G7).
	resendsMu sync.Mutex
	resends   map[string]resendBackoff
	clock     func() time.Time
	// store is the job store Serve runs over, read by NamesContent.
	storeMu sync.Mutex
	store   job.Store
	// slotsMu guards the per-server limits and queues (job_dispatch.go)
	// and the jobs this runtime is executing.
	slotsMu    sync.Mutex
	localJobs  int
	hostedJobs int
	hosts      map[string]*hostQueue
	executing  map[string]bool
}

// resendBackoff is when an uncertain operation may next be reconciled, and
// the wait that led there.
type resendBackoff struct {
	next  time.Time
	delay time.Duration
}

const (
	firstResendDelay = time.Second
	maxResendDelay   = time.Minute
	// requestIDWindow is how long a video provider holds a request_id after
	// its operation ends (INF-G5).
	requestIDWindow = 24 * time.Hour
)

func (e *JobExecution) now() time.Time {
	if e.clock != nil {
		return e.clock()
	}
	return time.Now()
}

// noteUncertain schedules the next reconciliation of id: 1 s after its first
// uncertain attempt, doubling to a 60 s cap.
func (e *JobExecution) noteUncertain(id string) {
	e.resendsMu.Lock()
	defer e.resendsMu.Unlock()
	if e.resends == nil {
		e.resends = map[string]resendBackoff{}
	}
	delay := firstResendDelay
	if last, ok := e.resends[id]; ok {
		delay = min(last.delay*2, maxResendDelay)
	}
	e.resends[id] = resendBackoff{next: e.now().Add(delay), delay: delay}
}

// noteAnswered resets id's backoff: the upstream answered.
func (e *JobExecution) noteAnswered(id string) {
	e.resendsMu.Lock()
	defer e.resendsMu.Unlock()
	delete(e.resends, id)
}

// resendDue reports whether id may be reconciled now.
func (e *JobExecution) resendDue(id string) bool {
	e.resendsMu.Lock()
	defer e.resendsMu.Unlock()
	backoff, waiting := e.resends[id]
	return !waiting || !e.now().Before(backoff.next)
}

// submitterPeer is the native peer a submission was bound from, kept in
// memory for that operation's decisions, and when it was kept.
type submitterPeer struct {
	peer *identity.Peer
	at   time.Time
}

// submitterPeerGrace is how long a kept peer outlives the absence of its
// operation from the store: preparation precedes the journal, and a sweep
// between them must not drop the peer of work about to be accepted.
const submitterPeerGrace = time.Minute

func (e *JobExecution) SetOperationSubjectResolver(resolve OperationJobSubjectResolver) {
	e.resolveOperationSubject = resolve
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
	maxEach, maxInput := int64(0), int64(0)
	if provider != nil {
		maxEach, maxInput = provider.cfg.MaxGeneratedImageBytes, provider.cfg.MaxImageBytes
	}
	backends := []durableJobBackend{
		nativeVideoJobBackend{maxInput: maxInput, maxOutput: maxBytes},
		replicateJobBackend{client: client, maxEach: maxEach, maxTotal: maxBytes},
	}
	return &JobExecution{provider: provider, resolveSubject: resolve, backends: backends, pollInterval: defaultInferenceJobPollInterval, leaseTTL: 30 * time.Second, onError: onError}
}

// backendFor returns the backend that serves request on a pinned host, and
// the profile it selected that host under.
func (e *JobExecution) backendFor(host *router.Host, request jobwire.Request) (durableJobBackend, string) {
	for _, backend := range e.backends {
		if profile := backend.Profile(request); profile != "" && backend.Supports(host, request) {
			return backend, profile
		}
	}
	return nil, ""
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
		// Edit mode names the clip's initial frame; a video has no mask.
		if request.DurationMs < 1 || request.DurationMs > 600000 || image.MaskDigest != "" || image.Count != 1 {
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

// PrepareForPeer is PrepareScoped for new work a local boundary bound from
// peer. It keeps peer in memory for id, and the executor's decisions for that
// operation carry it (WithPeer). The prepared work does not depend on peer,
// and an operation recovered after a restart carries none.
func (e *JobExecution) PrepareForPeer(scope, id, kind string, raw []byte, required []string, peer *identity.Peer) ([]byte, []string, error) {
	work, guarantees, err := e.PrepareScoped(scope, id, kind, raw, required)
	if err == nil && peer != nil {
		e.peersMu.Lock()
		if e.peers == nil {
			e.peers = map[string]submitterPeer{}
		}
		e.peers[id] = submitterPeer{peer: peer, at: time.Now()}
		e.peersMu.Unlock()
	}
	return work, guarantees, err
}

// submitter is the peer kept for operation id, or nil.
func (e *JobExecution) submitter(id string) *identity.Peer {
	e.peersMu.Lock()
	defer e.peersMu.Unlock()
	return e.peers[id].peer
}

// forgetSubmitters drops each peer older than submitterPeerGrace whose
// operation is not among active, the store's unfinished operations.
func (e *JobExecution) forgetSubmitters(active map[string]bool) {
	e.peersMu.Lock()
	defer e.peersMu.Unlock()
	for id, kept := range e.peers {
		if !active[id] && time.Since(kept.at) > submitterPeerGrace {
			delete(e.peers, id)
		}
	}
}

func (e *JobExecution) forgetSubmitter(id string) {
	e.peersMu.Lock()
	defer e.peersMu.Unlock()
	delete(e.peers, id)
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
	backend durableJobBackend
	// profile is the router profile the host was picked for, which its
	// admission reads (AdmissionProfile).
	profile string
	model   string
	adapter string
	headers map[string]string
	commits []ContentCommitter
	inputs  map[string]resolvedImageInput
}

func (e *JobExecution) plan(ctx context.Context, work inferenceJobWork, pin inferenceJobCheckpoint, operationID string) (inferenceJobPlan, acceptance.AcceptanceOutcome, string) {
	return e.planForSubject(ctx, work, pin, operationID, nil)
}

func (e *JobExecution) planForSubject(ctx context.Context, work inferenceJobWork, pin inferenceJobCheckpoint, operationID string, admitted *Subject) (inferenceJobPlan, acceptance.AcceptanceOutcome, string) {
	if e.provider == nil || e.resolveSubject == nil || len(e.backends) == 0 {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "provider:unavailable"
	}
	var subject Subject
	outcome := ContentResolved
	if admitted != nil {
		subject = *admitted
	} else if operationID != "" && e.resolveOperationSubject != nil {
		subject, outcome = e.resolveOperationSubject(ctx, operationID, work.Scope, work.Request.Image.Credential)
	} else {
		subject, outcome = e.resolveSubject(ctx, work.Scope, work.Request.Image.Credential)
	}
	switch outcome {
	case ContentResolved:
	case ContentForbidden, ContentUnknown:
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "caller:forbidden"
	default:
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "caller:unavailable"
	}
	request := imageWireRequest(work.Request.Image)
	var host *router.Host
	var backend durableJobBackend
	profile := ""
	model := pin.Model
	if pin.Phase == "" {
		reason := "router:" + router.NoHost
		for _, candidate := range e.backends {
			wanted := candidate.Profile(work.Request)
			if wanted == "" {
				continue
			}
			picked, pickedModel, why := e.provider.pick(ctx, request.Model, requestGuaranteeWords(request.Guarantees), request.Credential, wanted)
			if picked == nil {
				reason = why
				continue
			}
			host, model, backend, profile = picked, pickedModel, candidate, wanted
			break
		}
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
		backend, profile = e.backendFor(host, work.Request)
	}
	adapter := imageAdapterFor(host)
	if backend == nil || unsupportedImageFor(adapter, host, request, profile) != "" || !backend.Supports(host, work.Request) {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, "host:unsupported"
	}
	// Every grant the job needs is decided before the plan answers, and a job
	// missing several is refused once with every first-use question admitted
	// in the same pass (INF-V4, grantRefusal).
	var grants grantRefusal
	editInput := ""
	if request.Mode == wire.ImageModeEdit {
		editInput = InputImage
	}
	if !e.provider.decideComplete(ctx, subject, host, editInput, &grants) {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "rights:unavailable"
	}
	if grants.final {
		return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, grants.reason()
	}
	// A later check that cannot answer, or finds the request itself wrong,
	// stops the plan; a grant already refused is reported instead, since only
	// an answer to it can let the job run.
	stop := func(outcome acceptance.AcceptanceOutcome, reason string) (inferenceJobPlan, acceptance.AcceptanceOutcome, string) {
		if grants.refused() {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, grants.reason()
		}
		return inferenceJobPlan{}, outcome, reason
	}
	var inputs map[string]resolvedImageInput
	// Recovered work that holds a handle needs no input again. Work whose
	// submission may have reached the upstream is reconciled even when its
	// input can no longer be read: failing it here could leave the work
	// running upstream under a failed job (INF-G7).
	sent := pin.Phase != ""
	if request.Mode == wire.ImageModeEdit && pin.Handle == "" {
		input, resolved, why := e.provider.resolveImageInput(ctx, subject, request.ImageDigest, request.ImageMediaType)
		switch {
		case why == "":
			inputs = map[string]resolvedImageInput{request.ImageDigest: input}
		case sent && resolved != wire.StartOutcomeUnavailable:
			// Reconciled without the input; the backend names it unsent.
		case resolved == wire.StartOutcomeForbidden:
			grants.add(why)
		case resolved == wire.StartOutcomeUnavailable:
			return stop(acceptance.AcceptanceOutcomeUnavailable, why)
		default:
			return stop(acceptance.AcceptanceOutcomeInvalid, why)
		}
		if request.MaskDigest != "" && !sent {
			if _, resolved, why := e.provider.resolveImageInput(ctx, subject, request.MaskDigest, request.MaskMediaType); why != "" {
				switch {
				case resolved == wire.StartOutcomeForbidden:
					grants.add(why)
				case resolved == wire.StartOutcomeUnavailable:
					return stop(acceptance.AcceptanceOutcomeUnavailable, why)
				default:
					return stop(acceptance.AcceptanceOutcomeInvalid, why)
				}
			}
		}
	}
	// A video job's clip is its own output profile, an image batch's images
	// image's (INF-S7).
	output, media, maxOutput := OutputImage, "image/*", e.provider.cfg.MaxGeneratedImageBytes
	if work.Request.Profile == jobwire.ProfileVideo {
		output, media, maxOutput = OutputVideo, "video/*", e.provider.cfg.MaxImageOutputBytes
	}
	commits, ok := e.provider.prepareOutputs(ctx, subject, output, media, maxOutput, request.Count, &grants)
	if !ok {
		return stop(acceptance.AcceptanceOutcomeUnavailable, "output:unavailable")
	}
	if grants.refused() {
		return stop(acceptance.AcceptanceOutcomeForbidden, "")
	}
	credential := host.Credential
	if credential != "" && e.provider.cfg.Ceilings != nil {
		continuing := operationID != "" && e.provider.cfg.Ceilings.DurableAttemptCounted(operationID)
		if unit, over := e.provider.cfg.Ceilings.Exceeded(credential); over && !continuing {
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeInvalid, "ceiling:" + unit + ":" + credential
		}
	}
	var headers map[string]string
	var word string
	if credential != "" {
		headers, word = e.provider.cfg.Apply(ctx, subject, ImageContract, credential, router.Target(host.Base))
		if word != "applied" {
			if word == "unavailable" || word == "" {
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "credential:unavailable"
			}
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeForbidden, "credential:" + word
		}
	}
	return inferenceJobPlan{subject: subject, host: host, backend: backend, profile: profile, model: model, adapter: adapter, headers: headers, commits: commits, inputs: inputs}, acceptance.AcceptanceOutcomeAccepted, ""
}

func (e *JobExecution) CheckAdmission(scope, kind string, raw []byte, required []string) (acceptance.AcceptanceOutcome, string) {
	return admissionOutcome(e.checkAdmissionForSubject(scope, kind, raw, required, nil, nil))
}

// CheckAdmissionForSubject checks new work with the receiving boundary's
// account/program, before a journal or operation ID exists. peer is the
// native peer the boundary bound subject from, which the rights decision
// carries (WithPeer), or nil for a remote submitter.
func (e *JobExecution) CheckAdmissionForSubject(scope, kind string, raw []byte, required []string, subject Subject, peer *identity.Peer) (acceptance.AcceptanceOutcome, string) {
	return admissionOutcome(e.checkAdmissionForSubject(scope, kind, raw, required, &subject, peer))
}

// admissionOutcome is a plan's refusal as JOB-A16 admission answers it: a
// rights refusal stays forbidden with its reason, every grant the job lacks
// named once (INF-V4), and the job service seals nothing for it, so the same
// key is admitted once the person answers. A credential the applier refused
// for any reason other than its rights decision is the caller's to repair and
// reads invalid, as the download kind's does (download DL-K1).
func admissionOutcome(outcome acceptance.AcceptanceOutcome, reason string) (acceptance.AcceptanceOutcome, string) {
	if outcome == acceptance.AcceptanceOutcomeForbidden && strings.HasPrefix(reason, "credential:") && reason != "credential:not_permitted" {
		return acceptance.AcceptanceOutcomeInvalid, reason
	}
	return outcome, reason
}

func (e *JobExecution) checkAdmissionForSubject(scope, kind string, raw []byte, required []string, subject *Subject, peer *identity.Peer) (acceptance.AcceptanceOutcome, string) {
	if kind != InferenceJobKind || !slices.Contains(required, RecoverableUpstreamGuarantee) {
		return acceptance.AcceptanceOutcomeInvalid, "recoverable-upstream guarantee required"
	}
	request, err := decodeInferenceJob(raw)
	if err != nil {
		return acceptance.AcceptanceOutcomeInvalid, "invalid request"
	}
	ctx, cancel := context.WithTimeout(WithPeer(context.Background(), peer), 10*time.Second)
	defer cancel()
	_, outcome, reason := e.planForSubject(ctx, inferenceJobWork{Scope: scope, Request: request}, inferenceJobCheckpoint{}, "", subject)
	return outcome, reason
}

func (e *JobExecution) Serve(ctx context.Context, store job.Store) error {
	e.storeMu.Lock()
	e.store = store
	e.storeMu.Unlock()
	watch := job.Watch(store, InferenceJobKind)
	defer watch.Close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// Shutdown cancels ctx; each executing job then lets its lease go with
	// service:stopped, and Serve returns once all of them have.
	var running sync.WaitGroup
	defer running.Wait()
	for ctx.Err() == nil {
		e.sweep(ctx, store, &running)
		select {
		case <-ctx.Done():
			return nil
		case <-watch.Changes():
		case <-ticker.C:
		}
	}
	return nil
}

func (e *JobExecution) sweep(ctx context.Context, store job.Store, running *sync.WaitGroup) {
	e.peersMu.Lock()
	keeping := len(e.peers) > 0
	e.peersMu.Unlock()
	if e.provider.cfg.Ceilings != nil || keeping {
		if all, listErr := store.List(); listErr == nil {
			active := map[string]bool{}
			for _, record := range all {
				if record.Kind == InferenceJobKind && !record.State.Terminal() {
					active[record.ID] = true
				}
			}
			e.forgetSubmitters(active)
			if e.provider.cfg.Ceilings != nil {
				if err := e.provider.cfg.Ceilings.PruneDurable(active); err != nil && e.onError != nil {
					e.onError(fmt.Errorf("inference job accounting prune: %w", err))
				}
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
		// Uncertain work, a cancelled one included, waits out its backoff
		// unclaimed, holding no server slot: a cancel needs the operation the
		// next answer names.
		if !e.resendDue(record.ID) {
			continue
		}
		e.dispatch(ctx, store, record, running)
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

// stoppedOr is word, or service:stopped once serving, Serve's context, is
// done: a poll or rights decision that shutdown cancelled lets the job go
// with the shutdown's word, as one shutdown reaches between polls does
// (INF-G9).
func stoppedOr(serving context.Context, word string) string {
	if serving.Err() != nil {
		return "service:stopped"
	}
	return word
}

func (e *JobExecution) execute(serving context.Context, store job.Store, held *job.Record) error {
	ctx, lease := holdInferenceLease(serving, store, held, e.leaseTTL)
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
	cancelUnsent := func() error {
		lease.stop()
		if err := e.finish(store, held, job.StateCancelled, checkpoint); err != nil {
			return err
		}
		e.recordJobAttempt(ctx, held, work, inferenceJobPlan{}, "cancelled", "", 0)
		return nil
	}
	if held.Wants() == job.WantCancel && checkpoint.Phase == "" {
		return cancelUnsent()
	}
	// A cancel of work no upstream has answered within the request window
	// ends here: a provider returning later no longer recognises the
	// request_id, so no answer can name the operation to cancel (INF-G7).
	if held.Wants() == job.WantCancel && checkpoint.Phase != "" && checkpoint.Handle == "" && checkpoint.SentMs > 0 &&
		!e.now().Before(time.UnixMilli(checkpoint.SentMs).Add(requestIDWindow)) {
		lease.stop()
		checkpoint.Waiting, checkpoint.Failure = "", "upstream:unanswered"
		if err := e.finish(store, held, job.StateCancelled, checkpoint); err != nil {
			return err
		}
		e.recordJobAttempt(ctx, held, work, inferenceJobPlan{}, "cancelled", checkpoint.Failure, 0)
		return nil
	}
	plan, outcome, reason, leaveHost, cancelled := e.planOnHost(ctx, store, held, work, checkpoint)
	defer leaveHost()
	// A lost lease gives the server slot back at once, before the job's
	// remaining store calls, which a hung store can hold (INF-G9).
	go func() {
		select {
		case <-lease.lost:
			leaveHost()
		case <-lease.done:
		}
	}()
	if cancelled {
		return cancelUnsent()
	}
	if outcome != acceptance.AcceptanceOutcomeAccepted {
		if outcome == acceptance.AcceptanceOutcomeUnavailable && (reason == "host:replaced" || reason == "host:unavailable") {
			return e.recordedHostChanged(ctx, store, held, work, plan, checkpoint, reason, lease)
		}
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
	// The recorded server is back: an earlier outage is over, and the next
	// one counts its wait from its own start (INF-G10).
	checkpoint.HostLostMs = 0
	// The host is admitted as a Start's is (INF-A5, INF-G5): a declared
	// on-demand provider starts here, for a first submission and for work
	// recovered after a restart alike, before any call reaches it. A
	// provider still starting leaves the job waiting with the hook's word
	// (activating:<name>), and the next sweep asks again.
	if reason, ok := e.provider.admitHost(WithPeer(ctx, e.submitter(held.ID)), plan.host, plan.profile, plan.model); !ok {
		if reason == "not_permitted" {
			checkpoint.Failure = "admission:" + reason
			lease.stop()
			if err := e.finish(store, held, job.StateFailed, checkpoint); err != nil {
				return err
			}
			e.recordJobAttempt(ctx, held, work, plan, "failed", checkpoint.Failure, 0)
			return nil
		}
		if reason == "" {
			reason = "admission:unavailable"
		}
		return e.releaseWaitingRecorded(ctx, store, held, checkpoint, reason, work, plan)
	}
	freshSubmission := checkpoint.Phase == ""
	host := plan.host
	if e.provider.cfg.Ceilings != nil && host.Credential != "" {
		if err := e.provider.cfg.Ceilings.AddDurableAttempt(held.ID, host.Credential); err != nil {
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "ceiling:unavailable", work, plan)
		}
	}
	if freshSubmission {
		checkpoint = inferenceJobCheckpoint{Phase: "submitting", Host: plan.host.Name, Binding: inferenceHostBinding(plan.host), Model: plan.model, Adapter: plan.adapter,
			SentMs: e.now().UnixMilli(), Replanned: checkpoint.Replanned}
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
		submission := durableJobSubmission{host: host, operation: held.ID, model: checkpoint.Model, request: work.Request,
			headers: plan.headers, program: plan.subject.Program, inputs: plan.inputs}
		if freshSubmission {
			status, err = plan.backend.Submit(ctx, submission)
		} else {
			// A persisted submitting marker means the process may have reached
			// the paid upstream before it died. Only provider reconciliation may
			// recover it; replaying Submit can charge and produce twice.
			status, err = plan.backend.Reconcile(ctx, submission)
		}
		if err == nil && status.Handle == "" && status.State == durableJobFailed {
			// The upstream answered and refused: nothing runs there.
			e.noteAnswered(held.ID)
			checkpoint.Failure = status.Reason
			return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, 0, lease)
		}
		if err != nil || status.Handle == "" {
			e.noteUncertain(held.ID)
			checkpoint.Waiting = "upstream:uncertain"
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, checkpoint.Waiting, work, plan)
		}
		e.noteAnswered(held.ID)
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
			if cancelErr := plan.backend.Cancel(ctx, host, checkpoint.Handle, plan.headers); cancelErr != nil {
				return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "cancel:unavailable", work, plan)
			}
			return e.finishRecorded(store, held, job.StateCancelled, checkpoint, work, plan, 0, lease)
		}
		if status.State == "" || status.State == durableJobRunning {
			// Each poll is decided again, asking nobody, before the plan's
			// headers are sent (INF-A6). A refusal cancels the upstream work
			// and fails the job with the refusal; a decision that cannot be
			// obtained sends nothing, and the job waits to be planned again.
			refusal, standingErr := e.provider.standing(WithPeer(ctx, e.submitter(held.ID)), plan.subject, host.Name, host.Credential)
			if standingErr != nil {
				return e.releaseWaitingRecorded(ctx, store, held, checkpoint, stoppedOr(serving, "rights:unavailable"), work, plan)
			}
			if refusal != "" {
				if strings.HasPrefix(refusal, "credential:") && len(plan.headers) > 0 && e.provider.cfg.CancelledWith != nil {
					e.provider.cfg.CancelledWith(ctx, plan.subject, ImageContract, host.Credential, router.Target(host.Base))
				}
				if cancelErr := plan.backend.Cancel(ctx, host, checkpoint.Handle, plan.headers); cancelErr != nil && e.onError != nil {
					e.onError(fmt.Errorf("inference job %s: cancelling revoked work: %w", held.ID, cancelErr))
				}
				checkpoint.Failure = refusal
				return e.finishRecorded(store, held, job.StateFailed, checkpoint, work, plan, 0, lease)
			}
			status, err = plan.backend.Poll(ctx, host, checkpoint.Handle, work.Request, plan.headers)
			if err != nil {
				return e.releaseWaitingRecorded(ctx, store, held, checkpoint, stoppedOr(serving, "upstream:unavailable"), work, plan)
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
		// A running upstream's report becomes the snapshot's progress and
		// waiting word; an answer also clears a word left by an earlier outage.
		if word := status.Progress.waiting(); current.Progress.Done != status.Progress.Done || current.Progress.Total != status.Progress.Total || checkpoint.Waiting != word {
			checkpoint.Waiting = word
			progress := status.Progress
			held, err = store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error {
				record.State, record.Error = job.StateRunning, ""
				record.Progress.Done, record.Progress.Total, record.Progress.UpdatedAt = progress.Done, progress.Total, job.At(time.Now())
				return record.SetCheckpoint(checkpoint)
			})
			if err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, "service:stopped", work, plan)
		case <-time.After(e.pollWait(status.Progress)):
		}
	}
}

// pollWait is how long the executor waits before it reads a running upstream
// again: generatingInferenceJobPollInterval while the upstream reports steps
// of work under way, so each step reaches the job's progress, and the poll
// interval while it is queued or reports nothing, so a remote API is not
// asked more often than before. video@1's Observe has no wait for a change,
// so a shorter poll stands in for one (live walkthrough 2026-10-02, D10: a
// 5 s poll skipped steps of about 5.8 s).
func (e *JobExecution) pollWait(p durableJobProgress) time.Duration {
	every := e.pollInterval
	if every <= 0 {
		every = defaultInferenceJobPollInterval
	}
	if !p.Queued && p.Total > 0 && every > generatingInferenceJobPollInterval {
		return generatingInferenceJobPollInterval
	}
	return every
}

// inferenceHostBinding pins configuration, never secrets. It does not claim
// that an unchanged URL proves an upstream's continuity; a registered provider
// additionally supplies its service-owned BindingID.
func inferenceHostBinding(host *router.Host) string {
	//unchecked: the struct below holds only strings and a bool; json.Marshal on plain data types cannot fail
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
		if size == 0 || !generatedJobMedia(work.Request.Profile, output.MediaType) || size > perOutput || size > e.provider.cfg.MaxImageOutputBytes-total || http.DetectContentType(output.Data) != output.MediaType {
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

// recordedHostWait bounds how long a job waits for its recorded server to
// come back once it is gone (INF-G10). A variable so tests can shorten it.
var recordedHostWait = 30 * time.Minute

// recordedHostChanged handles a job sent to a server that is no longer the
// one it recorded (INF-G10). A server whose declaration was replaced, or one
// gone for recordedHostWait, cannot know the job's operation: a job that
// spends no credential is planned afresh once, with its same request_id, on
// whatever server the router picks now, and fails there with the plan's own
// reason when none serves it; a second change, or a job spending a
// credential, whose resend could be charged twice, fails as host:replaced or
// host:gone. A server gone for less than the wait leaves the job waiting as
// host:unavailable. No job waits on a changed server without end.
func (e *JobExecution) recordedHostChanged(ctx context.Context, store job.Store, held *job.Record, work inferenceJobWork, plan inferenceJobPlan, checkpoint inferenceJobCheckpoint, reason string, lease *inferenceLease) error {
	if reason == "host:unavailable" {
		now := e.now()
		if checkpoint.HostLostMs == 0 {
			checkpoint.HostLostMs = now.UnixMilli()
		}
		if now.Before(time.UnixMilli(checkpoint.HostLostMs).Add(recordedHostWait)) {
			return e.releaseWaitingRecorded(ctx, store, held, checkpoint, reason, work, plan)
		}
		reason = "host:gone"
	}
	if checkpoint.Replanned || work.Request.Image.Credential != "" {
		checkpoint.Waiting, checkpoint.Failure = "", reason
		lease.stop()
		if err := e.finish(store, held, job.StateFailed, checkpoint); err != nil {
			return err
		}
		e.recordJobAttempt(ctx, held, work, plan, "failed", reason, 0)
		return nil
	}
	return e.releaseWaitingRecorded(ctx, store, held, inferenceJobCheckpoint{Replanned: true}, reason+":replanned", work, plan)
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
	if err == nil {
		e.forgetSubmitter(held.ID)
		e.noteAnswered(held.ID)
	}
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
		if e.resolveOperationSubject != nil {
			subject, _ = e.resolveOperationSubject(ctx, held.ID, work.Scope, work.Request.Image.Credential)
		} else {
			subject, _ = e.resolveSubject(ctx, work.Scope, work.Request.Image.Credential)
		}
	}
	host, model, credential := "", work.Request.Image.Model, work.Request.Image.Credential
	if plan.host != nil {
		host, model, credential = plan.host.Name, plan.model, plan.host.Credential
	}
	profile := router.ProfileImage
	if work.Request.Profile == jobwire.ProfileVideo {
		profile = router.ProfileVideo
	}
	record := Record{Operation: held.ID, Profile: profile, Route: RouteNative, Account: subject.Account, Program: subject.Program,
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

// RefusedByRights reports a failed inference job that ended only because
// grants were refused: at its plan (INF-V4) or at a poll decided again
// (INF-A6). Every word of its failure is one of grantRefusals, words this
// executor writes for its own decisions; a word from a provider, under
// native:, upstream: or provider:, never is. The same request submitted
// again is then decided again rather than given this failure
// (abstraction-job JOB-A19).
func (e *JobExecution) RefusedByRights(record *job.Record) bool {
	if record.Kind != InferenceJobKind || record.State != job.StateFailed {
		return false
	}
	var checkpoint inferenceJobCheckpoint
	if record.DecodeCheckpoint(&checkpoint) != nil {
		return false
	}
	return grantRefusalWords(checkpoint.Failure)
}

// grantRefusals is the closed list of failure words this executor writes for
// a grant its own decision refused: complete on the server (INF-A2), the
// credential's use (INF-A6), the input it reads and the output it writes
// (INF-V4).
var grantRefusals = map[string]bool{
	"rights:denied":            true,
	"rights:not_granted":       true,
	"rights:unknown_action":    true,
	"credential:not_permitted": true,
	"content:read:forbidden":   true,
	"output:forbidden":         true,
}

// grantRefusalWords reports whether reason is one or more grant refusal
// words, every one of them from grantRefusals.
func grantRefusalWords(reason string) bool {
	words := strings.Fields(reason)
	for _, word := range words {
		if !grantRefusals[word] {
			return false
		}
	}
	return len(words) > 0
}

func (e *JobExecution) OperationFailure(record *job.Record) *acceptance.WorkFailure {
	if record.Kind != InferenceJobKind || record.State != job.StateFailed {
		return nil
	}
	var checkpoint inferenceJobCheckpoint
	//unchecked: a decode failure leaves checkpoint zero-valued; the following check already treats an empty Failure as "use the generic default message", which is the intended best-effort fallback here
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
