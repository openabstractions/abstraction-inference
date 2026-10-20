// Package service hosts abstraction.inference/chat@1, and operator@1 when an
// operator is composed, on one shared framed IPC endpoint, binding every caller
// by native Program proof.
package service

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	asks "github.com/openabstractions/abstraction-asks/go"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

// MaxFrameBytes bounds one request frame; a Request fits one control frame.
const MaxFrameBytes = 1 << 20

// callBudget covers the longest Observe wait and its reply.
const callBudget = 35 * time.Second

// questionHold bounds how long one native admission holds for the person's
// answer to the first-use questions its refusal raised. A frame carries no
// client deadline, and every generated client's default call budget is
// listen.DefaultFrameTimeout, so the hold ends with time left to deliver the
// refusal that names each pending question (INF-W11). A longer wait is the
// caller's, through asks permission@1 ObserveQuestion.
const questionHold = listen.DefaultFrameTimeout - 1500*time.Millisecond

// OperatorContract is the wire contract of the operator profile.
const OperatorContract = "abstraction.inference/operator@1"

const SelectionOperatorV2Contract = "abstraction.inference/selection-operator@2"

type SelectionOperatorV2 interface {
	ReadEffective(ctx context.Context, caller inference.Subject, name string) wire.SelectionReadV2
	ReadSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, name string) wire.SelectionReadV2
	PutSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, expectedRevision string, policy wire.SelectionPolicyV2) wire.SelectionChange
	DeleteSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, expectedRevision, name string) wire.SelectionChange
	ReadAccountDefault(ctx context.Context, caller inference.Subject, name string) wire.SelectionReadV2
	PutAccountDefault(ctx context.Context, caller inference.Subject, expectedRevision string, policy wire.SelectionPolicyV2) wire.SelectionChange
	DeleteAccountDefault(ctx context.Context, caller inference.Subject, expectedRevision, name string) wire.SelectionChange
}

// Operator serves abstraction.inference/operator@1 for a bound subject. Each
// method decides its rights action itself; the zero Subject is a caller the
// boundary could not bind and reads forbidden.
type Operator interface {
	Hosts(ctx context.Context, caller inference.Subject) wire.HostList
	AddHost(ctx context.Context, caller inference.Subject, expectedRevision string, host wire.HostEntry) wire.HostChange
	RemoveHost(ctx context.Context, caller inference.Subject, expectedRevision, name string) wire.HostChange
	Keys(ctx context.Context, caller inference.Subject) wire.KeyList
	IssueKey(ctx context.Context, caller inference.Subject, program, credential string) wire.KeyIssued
	RevokeKey(ctx context.Context, caller inference.Subject, program string) wire.KeyRevoked
	Audit(ctx context.Context, caller inference.Subject, cursor, maxEntries int64) wire.AuditPage
	Gateway(ctx context.Context, caller inference.Subject) wire.GatewayState
	SetGateway(ctx context.Context, caller inference.Subject, expectedRevision string, open bool, address string) wire.GatewayChange
}

type Host struct {
	lifecycle sync.Mutex
	serving   bool
	// callerGone is set once a reply found its caller closed and was
	// reported; later such replies go unreported until one is delivered.
	callerGone atomic.Bool
	admission sync.RWMutex
	retired   bool
	listener  listen.Listener
	owner     string
	provider  *inference.Provider
	placement inference.ExecutionPlacement
	binding   inference.ExecutionBinding
	leaf      bool
	// designated limits a leaf provider that delegates authorization to the
	// runtime. An ordinary inference service leaves it empty.
	designated []string
	ctx        context.Context
	cancel     context.CancelFunc
	once       sync.Once
	workers    sync.WaitGroup
	slots      listen.CallAdmission
	// ApplicationChatV3, assigned before Serve, exposes chat@3 on an ordinary
	// local application endpoint. Trusted composition binds its logical owner
	// before advertising it. Leaf, designated, bound and other placements refuse.
	ApplicationChatV3 bool
	// AwaitQuestions, assigned before Serve, may wait for questions gathered
	// by a refused synchronous native preflight. A true result retries only
	// that no-effect preflight within the same frame. Nil preserves immediate
	// refusal. Accepted work, durable jobs and video never enter this callback.
	AwaitQuestions func(context.Context, inference.Subject, []string) (bool, error)
	// PendingQuestions, assigned before Serve, returns which of the gathered
	// questions are still pending about the caller once AwaitQuestions
	// released no retry. The refusal names each as rights:question_pending:<id>
	// after its grant words (INF-W11). Nil names none.
	PendingQuestions func(context.Context, inference.Subject, []string) ([]string, error)
	OnError          func(error)
	// Assign before Serve. Called when admission stops, before calls drain.
	OnStopped func()
	// Operator, assigned before Serve, serves operator@1 on the same endpoint.
	// Nil refuses that profile.
	Operator            Operator
	SelectionOperatorV2 SelectionOperatorV2
	// Content, assigned before Serve, composes authenticated content services
	// into this same endpoint for an independent provider store.
	Content *storageService.Component
	// Video, assigned before Serve, serves video@1 on this same endpoint
	// (INF-G5). Nil refuses that profile.
	Video VideoServer
	// ProducerChecks, assigned before Serve, returns captured program-level
	// observations. Nil leaves checks unobserved. Describe never repairs or
	// infers a successful check from service readiness.
	ProducerChecks func() []wire.EndpointProducerCheck
	// Residents, assigned before Serve, names the models the provider holds
	// loaded now. Describe states them on chat@1 as the router's resident
	// display fact
	// (router.ResidentCapabilities), which the router reads as this native
	// host's residency. Nil states none, and residency stays unreported.
	Residents func() []string
	// ResidentContexts, assigned before Serve, names the per-conversation
	// context each resident model was loaded with. Describe states it beside
	// the resident names (router.ResidentContextCapabilities), and the
	// router reports it as the model's effective context. Nil states none.
	ResidentContexts func() map[string]int64
	// ModelCapabilities, assigned before Serve, names each served model's
	// capabilities in order. Describe states them on chat@3 as display
	// facts (router.CapabilityFacts), which the runtime's router reports as
	// each alias's capabilities. Nil states none.
	ModelCapabilities func() ([]string, map[string]*router.ModelCapabilities)
}

// VideoServer is a native provider's video@1 (INF-G5). The host binds every
// call's peer and answers forbidden itself for a caller it does not admit.
// On a designated leaf only the designated runtime binds, and every call's
// caller is the bound subject with Program set to the runtime's designation,
// the leaf's first designated program, whichever designated executable
// connected. The same caller holds a request_id and owns an operation for
// every executable the leaf designates (I-12).
type VideoServer interface {
	Start(ctx context.Context, caller inference.Subject, request wire.VideoRequest) wire.VideoAdmission
	Observe(ctx context.Context, caller inference.Subject, operation string) wire.VideoObservation
	Cancel(ctx context.Context, caller inference.Subject, operation string) wire.Cancellation
}

// Listen serves provider on endpoint for callers of the runtime's account.
func Listen(endpoint string, provider *inference.Provider) (*Host, error) {
	return listenForPlacement(endpoint, provider, inference.ExecutionAny, inference.ExecutionBinding{}, false)
}

// ListenForPlacement serves an endpoint whose execution placement is fixed
// before any caller can connect. Requests cannot widen this policy.
func ListenForPlacement(endpoint string, provider *inference.Provider, placement inference.ExecutionPlacement) (*Host, error) {
	return listenForPlacement(endpoint, provider, placement, inference.ExecutionBinding{}, false)
}

// ListenLeaf serves a one-hop provider endpoint. Calls admitted here may use
// leaf backends and refuse another native or remote OA delegation.
func ListenLeaf(endpoint string, provider *inference.Provider, placement inference.ExecutionPlacement) (*Host, error) {
	return listenForPlacement(endpoint, provider, placement, inference.ExecutionBinding{}, true)
}

// ListenDesignatedLeaf serves a one-hop provider that trusts only the named
// runtime executables to have authorized the original application. A missing
// designation refuses construction before opening the endpoint.
func ListenDesignatedLeaf(endpoint string, provider *inference.Provider, placement inference.ExecutionPlacement, programs ...string) (*Host, error) {
	if len(programs) == 0 {
		return nil, errors.New("inference service: runtime program required")
	}
	allowed := make([]string, 0, len(programs))
	for _, program := range programs {
		if !identity.ValidSubjectProgram(program) || !filepath.IsAbs(program) {
			return nil, errors.New("inference service: runtime program must be an absolute executable path")
		}
		allowed = append(allowed, identity.CanonicalProgramPath(program))
	}
	h, err := listenForPlacement(endpoint, provider, placement, inference.ExecutionBinding{}, true)
	if err != nil {
		return nil, err
	}
	h.designated = allowed
	return h, nil
}

// ListenMediated serves one declaration generation selected by resolution.
// The endpoint refuses another host or a replacement with the same name.
func ListenMediated(endpoint string, provider *inference.Provider, placement inference.ExecutionPlacement, binding inference.ExecutionBinding) (*Host, error) {
	if binding.Host == "" || binding.BindingID == "" {
		return nil, errors.New("inference service: mediated binding required")
	}
	return listenForPlacement(endpoint, provider, placement, binding, false)
}

func listenForPlacement(endpoint string, provider *inference.Provider, placement inference.ExecutionPlacement, binding inference.ExecutionBinding, leaf bool) (*Host, error) {
	if provider == nil {
		return nil, errors.New("inference service: provider required")
	}
	if placement != inference.ExecutionAny && placement != inference.ExecutionLocal && placement != inference.ExecutionRemote {
		return nil, errors.New("inference service: invalid execution placement")
	}
	owner, err := user.Current()
	if err != nil {
		return nil, err
	}
	if owner.Uid == "" {
		return nil, errors.New("inference service: service principal unavailable")
	}
	l, err := listen.ListenFramed(endpoint, listen.Program)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{listener: listen.Sessions(l, listen.SessionOptions{MaxSessions: 64}), owner: owner.Uid, provider: provider, placement: placement, binding: binding, leaf: leaf, ctx: ctx, cancel: cancel, slots: listen.NewCallAdmission(64)}, nil
}

func (h *Host) Close() error {
	var err error
	h.once.Do(func() {
		h.cancel()
		err = h.listener.Close()
		if h.Content != nil {
			if contentErr := h.Content.Close(); err == nil {
				err = contentErr
			}
		}
	})
	return err
}

// RetireAdmissions refuses future Start calls and waits for an admission
// already inside the provider boundary to finish. Observe and Cancel remain
// available for operations accepted before retirement.
func (h *Host) RetireAdmissions() {
	h.admission.Lock()
	h.retired = true
	h.admission.Unlock()
}

func (h *Host) Serve(ctx context.Context) error {
	h.lifecycle.Lock()
	if h.serving {
		h.lifecycle.Unlock()
		return errors.New("inference service: host already served")
	}
	if h.ApplicationChatV3 && (h.leaf || len(h.designated) != 0 || h.binding != (inference.ExecutionBinding{}) || h.placement != inference.ExecutionLocal) {
		h.lifecycle.Unlock()
		return errors.New("inference service: application chat@3 requires an unbound ordinary local host")
	}
	h.serving = true
	h.lifecycle.Unlock()
	if h.Content != nil {
		if err := h.Content.Start(h.ctx); err != nil {
			_ = h.Close()
			return err
		}
	}
	//unchecked: best-effort shutdown triggered asynchronously by context cancellation; Serve already returns its own error and has no way to receive a second one from this later callback
	stop := context.AfterFunc(ctx, func() { h.Close() })
	defer stop()
	defer h.workers.Wait()
	defer func() {
		if h.OnStopped != nil {
			h.OnStopped()
		}
	}()
	defer h.Close()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || h.ctx.Err() != nil {
				return nil
			}
			return err
		}
		release, admitted := h.slots.Take(conn)
		if !admitted {
			continue
		}
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			defer release()
			defer conn.Close()
			accepted := time.Now()
			callCtx, cancel := context.WithTimeout(h.ctx, callBudget)
			defer cancel()
			call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, MaxFrameBytes)
			if call != nil {
				defer call.Close()
			}
			if err == nil {
				base := &receiver{host: h, call: call, ctx: callCtx, accepted: accepted}
				services := []wire.ServedService{&wire.ChatDispatcher{Handler: chatReceiver{base}}, &wire.ChatV2Dispatcher{Handler: &chatV2Receiver{base}}, &wire.EmbedderDispatcher{Handler: base}, &wire.PreloaderDispatcher{Handler: base}, &wire.TranscriptionDispatcher{Handler: &transcriptionReceiver{base}}, &wire.SpeechDispatcher{Handler: &speechReceiver{base}}, &wire.ImageDispatcher{Handler: &imageReceiver{base}}, &wire.ImageV2Dispatcher{Handler: &imageV2Receiver{base}}}
				if h.leaf || h.ApplicationChatV3 {
					services = append(services, &wire.ChatV3Dispatcher{Handler: &chatV3Receiver{base}})
				}
				services = append(services, &wire.LiveDispatcher{Handler: &liveReceiver{base}})
				if h.Content != nil {
					for _, contentService := range h.Content.Services(call, callCtx) {
						services = append(services, contentService)
					}
				}
				if h.Video != nil {
					services = append(services, &wire.VideoDispatcher{Handler: &videoReceiver{base}})
				}
				if h.Operator != nil {
					services = append(services, &wire.OperatorDispatcher{Handler: &operatorReceiver{base}})
				}
				if h.SelectionOperatorV2 != nil {
					services = append(services, &wire.SelectionOperatorV2Dispatcher{Handler: &selectionOperatorV2Receiver{base}})
				}
				if h.binding.Host != "" && h.placement == inference.ExecutionRemote {
					// The paired first profile offers exactly chat and embeddings.
					// Keep other local services outside this selected peer endpoint.
					services = []wire.ServedService{&wire.ChatDispatcher{Handler: base}, &wire.EmbedderDispatcher{Handler: base}}
				}
				var reply []byte
				reply, err = h.serveEndpoint(call.Frame, services...)
				if err == nil {
					err = call.Reply(reply)
				}
			}
			h.reportCall(err)
		}()
	}
}

// reportCall hands one call's failure to OnError. A reply whose caller had
// already closed is reported once; the next ones are not until a reply is
// delivered again. On R28 a run of runtime probes that all timed out during
// one model load each logged "write pipe: The pipe is being closed."
func (h *Host) reportCall(err error) {
	if err == nil {
		h.callerGone.Store(false)
		return
	}
	if h.OnError == nil || h.ctx.Err() != nil {
		return
	}
	if listen.CallerClosed(err) {
		if h.callerGone.CompareAndSwap(false, true) {
			h.OnError(fmt.Errorf("%w (the caller had closed; further replies to closed callers are not reported until one is delivered)", err))
		}
		return
	}
	h.OnError(err)
}

func (h *Host) serveEndpoint(frame []byte, services ...wire.ServedService) ([]byte, error) {
	contract, err := wire.ServiceName(frame)
	if err != nil {
		return nil, err
	}
	var checks []wire.EndpointProducerCheck
	if contract == wire.EndpointV2Contract && h.ProducerChecks != nil {
		checks = h.ProducerChecks()
	}
	return wire.ServeEndpointWithChecks(frame, "openabstractions", "", checks, services...)
}

// SubjectFromPeer is identity.SubjectOf at listen.Program, in the inference
// contract's Subject.
func SubjectFromPeer(peer *identity.Peer) (inference.Subject, error) {
	s, err := identity.SubjectOf(peer, listen.Program)
	if err != nil {
		return inference.Subject{}, err
	}
	return inference.Subject{Account: s.Account, Program: s.Program}, nil
}

type receiver struct {
	host *Host
	call *listen.FramedCall
	ctx  context.Context
	// accepted is when the connection was accepted; the caller's default
	// call budget runs from about then.
	accepted time.Time
}

// chatReceiver is chat@1's handler: the receiver, and the provider's
// resident models on Describe.
type chatReceiver struct{ *receiver }

// DescribeMetadata states the provider's resident models for Describe; it
// grants nothing and changes no admission.
func (r chatReceiver) DescribeMetadata() ([]string, map[string]string) {
	if r.host == nil || r.host.Residents == nil {
		return nil, nil
	}
	residents := r.host.Residents()
	var contexts map[string]int64
	if r.host.ResidentContexts != nil {
		contexts = r.host.ResidentContexts()
	}
	return nil, router.ResidentContextCapabilities(residents, contexts)
}

// admittingContext carries the endpoint's fixed policy and the bound peer
// into admission. Decide reads the peer the subject was bound from
// (inference.PeerFromContext); an unbound caller carries none. A designated
// leaf marks a bound caller as its designated runtime, whose caller claim the
// provider records; every other caller's claim is discarded.
func (r *receiver) admittingContext(subject inference.Subject, rung string, peer *identity.Peer) context.Context {
	call := r.ctx
	if r.call != nil {
		// Pending admission has no durable operation yet. Reuse the framed
		// call's deadline and caller-close cancellation, as Observe does.
		call = r.call.WaitContext()
	}
	ctx := inference.WithExecutionPlacement(inference.WithRoute(call, inference.RouteNative, rung), r.host.placement)
	ctx = inference.WithPeer(ctx, peer)
	if r.call != nil && !r.accepted.IsZero() {
		// The frame carries no client deadline; the caller's default call
		// budget, the one questionHold also keeps within, bounds admission.
		ctx = inference.WithReplyDeadline(ctx, r.accepted.Add(listen.DefaultFrameTimeout))
	}
	if r.call != nil && (r.host.AwaitQuestions != nil || r.host.PendingQuestions != nil) {
		// Private native/runtime composition: this collector belongs to this
		// active framed admission. A generic Asked or copied peer proves none.
		asked := &inference.Asked{}
		ctx = inference.WithAsked(ctx, asked)
		ctx = context.WithValue(ctx, asked, r.call)
	}
	if r.host.binding.Host != "" {
		ctx = inference.WithExecutionBinding(ctx, r.host.binding)
	}
	// An ordinary native caller's hosted-allowed request that names no
	// credential reaches each hosted model server under the credential the
	// server's declaration names, as the gateway window's placeholder key
	// does (INF-A3, INF-W5). The program's complete and credentials/apply
	// rights decide each use, and their first-use question holds the call.
	if r.host.placement != inference.ExecutionLocal && r.host.binding.Host == "" && !r.host.leaf && len(r.host.designated) == 0 {
		ctx = inference.WithDeclaredCredential(ctx)
	}
	if r.host.leaf {
		ctx = inference.WithLeafExecution(ctx)
	}
	if len(r.host.designated) > 0 && subject.Program != "" {
		ctx = inference.WithDesignatedRuntimeRecheck(ctx, r.call.Recheck)
	}
	return ctx
}

// awaitAdmission re-enters only a locally questioned permission refusal.
// These refusal words are produced before provider admission or upstream work;
// an accepted, unavailable, unknown or other result is never replayed. The
// hold for the person ends after questionHold in all; the refusal then names
// each question still pending about the caller (INF-W11).
func (r *receiver) awaitAdmission(subject inference.Subject, rung string, peer *identity.Peer, attempt func(context.Context) (string, *string)) error {
	holdUntil := time.Now().Add(questionHold)
	for {
		ctx := r.admittingContext(subject, rung, peer)
		outcome, reason := attempt(ctx)
		if r.host.AwaitQuestions == nil && r.host.PendingQuestions == nil || reason == nil || !pendingPermissionRefusal(outcome, *reason) {
			return nil
		}
		asked := inference.AskedFrom(ctx)
		if asked == nil || len(asked.IDs()) == 0 {
			return nil
		}
		if r.call == nil {
			return errors.New("native admission frame unavailable")
		}
		if err := r.call.Recheck(); err != nil {
			return err
		}
		if r.host.AwaitQuestions == nil {
			return r.namePending(ctx, subject, asked.IDs(), reason)
		}
		hold, release := context.WithDeadline(context.WithValue(ctx, admissionKey{}, ctx), holdUntil)
		retry, err := r.host.AwaitQuestions(hold, subject, asked.IDs())
		held := hold.Err() != nil
		release()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil && !held {
			return err
		}
		if !retry || err != nil {
			return r.namePending(ctx, subject, asked.IDs(), reason)
		}
		if err := r.call.Recheck(); err != nil {
			return err
		}
	}
}

// admissionKey carries the admitting context into the bounded hold, whose
// own lifetime is shorter.
type admissionKey struct{}

// AdmissionContext is the admitting context of the bounded hold ctx was
// derived from by AwaitQuestions' caller, or ctx itself. A hook that checks
// the frame binding reads it there and waits on ctx.
func AdmissionContext(ctx context.Context) context.Context {
	if admitting, ok := ctx.Value(admissionKey{}).(context.Context); ok {
		return admitting
	}
	return ctx
}

// namePending appends one rights:question_pending:<id> word for each gathered
// question still pending about subject.
func (r *receiver) namePending(ctx context.Context, subject inference.Subject, ids []string, reason *string) error {
	if r.host.PendingQuestions == nil {
		return nil
	}
	pending, err := r.host.PendingQuestions(ctx, subject, ids)
	if err != nil {
		return err
	}
	if words := questionWords(pending); words != "" {
		*reason += " " + words
	}
	return nil
}

// questionPendingPrefix begins the refusal word naming one pending first-use
// question (INF-W11).
const questionPendingPrefix = "rights:question_pending:"

// questionWords names each pending question id once, in order.
func questionWords(ids []string) string {
	words := make([]string, 0, len(ids))
	for _, id := range ids {
		word := questionPendingPrefix + id
		if id != "" && !strings.ContainsAny(id, " \t\r\n") && !slices.Contains(words, word) {
			words = append(words, word)
		}
	}
	return strings.Join(words, " ")
}

// pendingPermissionRefusal reports whether reason is grant refusal words,
// optionally followed by the words naming their pending questions.
func pendingPermissionRefusal(outcome, reason string) bool {
	if outcome != "not_permitted" && outcome != "forbidden" {
		return false
	}
	grant := false
	for _, word := range strings.Fields(reason) {
		switch {
		case word == "rights:not_granted" || word == "content:read:forbidden" || word == "output:forbidden" || word == "credential:not_permitted" || strings.HasPrefix(word, "credential:not_permitted:"):
			grant = true
		case len(asks.QuestionsPending(word)) != 0:
		default:
			return false
		}
	}
	return grant
}

// caller returns the bound subject of this runtime's account. A transport that
// cannot satisfy Program proof yields the zero Subject, refused as forbidden.
func (r *receiver) caller() inference.Subject {
	subject, _, _ := r.bound()
	return subject
}

// bound returns the caller's subject, the rung it was bound at and the peer it
// was bound from. A caller this endpoint does not admit returns the zero
// Subject and no peer.
func (r *receiver) bound() (inference.Subject, string, *identity.Peer) {
	peer, err := r.call.Peer()
	if err != nil {
		return inference.Subject{}, "", nil
	}
	subject, verdict := identity.AdmitOwner(peer, listen.Program, r.host.owner)
	if verdict != identity.OwnerAdmitted || r.call.Recheck() != nil {
		return inference.Subject{}, peer.Rung(), nil
	}
	if len(r.host.designated) > 0 {
		matched := false
		for _, allowed := range r.host.designated {
			if identity.SameSubjectProgram(subject.Program, allowed) {
				matched = true
				break
			}
		}
		if !matched {
			return inference.Subject{}, peer.Rung(), nil
		}
	}
	return inference.Subject{Account: subject.Account, Program: subject.Program}, peer.Rung(), peer
}

func (r *receiver) Start(request wire.Request) (wire.Admission, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.Admission{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung, peer := r.bound()
	var result wire.Admission
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.Start(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

func (r *receiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.Observe(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *receiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.Cancel(r.caller(), operation), nil
}

type chatV2Receiver struct{ *receiver }

func (r *chatV2Receiver) Start(request wire.RequestV2) (wire.AdmissionV2, error) {
	return r.start(request, nil)
}

func (r *chatV2Receiver) StartWithGuarantees(request wire.RequestV2, guarantees []wire.RequestGuarantee) (wire.AdmissionV2, error) {
	return r.start(request, guarantees)
}

func (r *chatV2Receiver) start(request wire.RequestV2, guarantees []wire.RequestGuarantee) (wire.AdmissionV2, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.AdmissionV2{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung, peer := r.bound()
	var result wire.AdmissionV2
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartV2WithGuarantees(ctx, subject, request, guarantees)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

// Preload admits like Start: a retired endpoint admits no new preload.
func (r *receiver) Preload(request wire.Request) (wire.PreloadReply, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung, peer := r.bound()
	var result wire.PreloadReply
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.Preload(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

func (r *receiver) Embed(request wire.EmbedRequest) (wire.Embeddings, error) {
	subject, rung, peer := r.bound()
	var result wire.Embeddings
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.Embed(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.Embeddings{Outcome: wire.EmbedOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

type transcriptionReceiver struct{ *receiver }

func (r *transcriptionReceiver) Start(request wire.TranscriptionRequest) (wire.Admission, error) {
	subject, rung, peer := r.bound()
	var result wire.Admission
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartTranscription(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

type speechReceiver struct{ *receiver }

// videoReceiver hands each video@1 call to the host's VideoServer with the
// bound caller. A retired endpoint admits no new video, as it admits no
// other Start. video@1 is a provider's service for its designated runtime
// (INF-G5): an endpoint that designates none answers every caller
// forbidden.
type videoReceiver struct{ *receiver }

// videoCaller is the bound designated runtime, or the zero Subject. Its
// Program is the runtime's designation, the leaf's first designated
// executable, for every executable the leaf designates: a job one runtime
// executable started and another reclaims is the same caller's (I-12).
func (r *videoReceiver) videoCaller() (inference.Subject, string, *identity.Peer) {
	if len(r.host.designated) == 0 {
		return inference.Subject{}, "", nil
	}
	subject, rung, peer := r.bound()
	if subject.Program != "" {
		subject.Program = r.host.designated[0]
	}
	return subject, rung, peer
}

func (r *videoReceiver) Start(request wire.VideoRequest) (wire.VideoAdmission, error) {
	r.host.admission.RLock()
	defer r.host.admission.RUnlock()
	if r.host.retired {
		return wire.VideoAdmission{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	subject, rung, peer := r.videoCaller()
	if subject.Program == "" {
		return wire.VideoAdmission{Outcome: wire.StartOutcomeForbidden}, nil
	}
	return r.host.Video.Start(r.admittingContext(subject, rung, peer), subject, request), nil
}

func (r *videoReceiver) Observe(operation string) (wire.VideoObservation, error) {
	subject, _, _ := r.videoCaller()
	if subject.Program == "" {
		return wire.VideoObservation{Status: wire.VideoStateForbidden}, nil
	}
	return r.host.Video.Observe(r.ctx, subject, operation), nil
}

func (r *videoReceiver) Cancel(operation string) (wire.Cancellation, error) {
	subject, _, _ := r.videoCaller()
	if subject.Program == "" {
		return wire.Cancellation{Outcome: wire.CancelOutcomeForbidden}, nil
	}
	return r.host.Video.Cancel(r.ctx, subject, operation), nil
}

type imageReceiver struct{ *receiver }

func (r *imageReceiver) Start(request wire.ImageRequest) (wire.Admission, error) {
	subject, rung, peer := r.bound()
	var result wire.Admission
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartImage(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}
func (r *imageReceiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveImage(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *imageReceiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelImage(r.caller(), operation), nil
}

func (r *speechReceiver) Start(request wire.SpeechRequest) (wire.Admission, error) {
	subject, rung, peer := r.bound()
	var result wire.Admission
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartSpeech(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}
func (r *speechReceiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveSpeech(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *speechReceiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelSpeech(r.caller(), operation), nil
}

func (r *transcriptionReceiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveTranscription(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *transcriptionReceiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelTranscription(r.caller(), operation), nil
}

type operatorReceiver struct{ *receiver }

type selectionOperatorV2Receiver struct{ *receiver }

func (r *selectionOperatorV2Receiver) ReadEffective(name string) (wire.SelectionReadV2, error) {
	return r.host.SelectionOperatorV2.ReadEffective(r.ctx, r.caller(), name), nil
}

func (r *selectionOperatorV2Receiver) Read(targetProgram, name string) (wire.SelectionReadV2, error) {
	return r.host.SelectionOperatorV2.ReadSelectionV2(r.ctx, r.caller(), targetProgram, name), nil
}
func (r *selectionOperatorV2Receiver) Put(targetProgram, expectedRevision string, policy wire.SelectionPolicyV2) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.PutSelectionV2(r.ctx, r.caller(), targetProgram, expectedRevision, policy), nil
}
func (r *selectionOperatorV2Receiver) DeletePolicy(targetProgram, expectedRevision, name string) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.DeleteSelectionV2(r.ctx, r.caller(), targetProgram, expectedRevision, name), nil
}

func (r *selectionOperatorV2Receiver) ReadAccountDefault(name string) (wire.SelectionReadV2, error) {
	return r.host.SelectionOperatorV2.ReadAccountDefault(r.ctx, r.caller(), name), nil
}
func (r *selectionOperatorV2Receiver) PutAccountDefault(expectedRevision string, policy wire.SelectionPolicyV2) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.PutAccountDefault(r.ctx, r.caller(), expectedRevision, policy), nil
}
func (r *selectionOperatorV2Receiver) DeleteAccountDefault(expectedRevision, name string) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.DeleteAccountDefault(r.ctx, r.caller(), expectedRevision, name), nil
}

func (r *operatorReceiver) Hosts() (wire.HostList, error) {
	return r.host.Operator.Hosts(r.ctx, r.caller()), nil
}

func (r *operatorReceiver) AddHost(expectedRevision string, host wire.HostEntry) (wire.HostChange, error) {
	// The bound peer rides along: a host add from a program other than the
	// person's own window is a request the person confirms (rights
	// RIGHTS-O17), and the runtime reads the peer to tell them apart.
	subject, _, peer := r.bound()
	return r.host.Operator.AddHost(inference.WithPeer(r.ctx, peer), subject, expectedRevision, host), nil
}

func (r *operatorReceiver) RemoveHost(expectedRevision, name string) (wire.HostChange, error) {
	return r.host.Operator.RemoveHost(r.ctx, r.caller(), expectedRevision, name), nil
}

func (r *operatorReceiver) Keys() (wire.KeyList, error) {
	return r.host.Operator.Keys(r.ctx, r.caller()), nil
}

func (r *operatorReceiver) IssueKey(program, credential string) (wire.KeyIssued, error) {
	return r.host.Operator.IssueKey(r.ctx, r.caller(), program, credential), nil
}

func (r *operatorReceiver) RevokeKey(program string) (wire.KeyRevoked, error) {
	return r.host.Operator.RevokeKey(r.ctx, r.caller(), program), nil
}

func (r *operatorReceiver) Audit(cursor, maxEntries int64) (wire.AuditPage, error) {
	return r.host.Operator.Audit(r.ctx, r.caller(), cursor, maxEntries), nil
}

func (r *operatorReceiver) Gateway() (wire.GatewayState, error) {
	return r.host.Operator.Gateway(r.ctx, r.caller()), nil
}

func (r *operatorReceiver) SetGateway(expectedRevision string, open bool, address string) (wire.GatewayChange, error) {
	return r.host.Operator.SetGateway(r.ctx, r.caller(), expectedRevision, open, address), nil
}
