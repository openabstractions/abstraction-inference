// Package service hosts abstraction.inference/chat@1, and operator@1 when an
// operator is composed, on one shared framed IPC endpoint, binding every caller
// by native Program proof.
package service

import (
	"context"
	"errors"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

// MaxFrameBytes bounds one request frame; a Request fits one control frame.
const MaxFrameBytes = 1 << 20

// callBudget covers the longest Observe wait and its reply.
const callBudget = 35 * time.Second

// OperatorContract is the wire contract of the operator profile.
const OperatorContract = "abstraction.inference/operator@1"

const SelectionOperatorV2Contract = "abstraction.inference/selection-operator@2"

type SelectionOperatorV2 interface {
	ReadSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, name string) wire.SelectionReadV2
	PutSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, expectedRevision string, policy wire.SelectionPolicyV2) wire.SelectionChange
	DeleteSelectionV2(ctx context.Context, caller inference.Subject, targetProgram, expectedRevision, name string) wire.SelectionChange
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
	slots      chan struct{}
	OnError    func(error)
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
}

// VideoServer is a native provider's video@1 (INF-G5). The host binds every
// call's peer as it binds chat and answers forbidden itself for a caller it
// does not admit; caller is always a bound subject. On a designated leaf
// only the designated runtime binds.
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
	return &Host{listener: listen.Sessions(l, listen.SessionOptions{MaxSessions: 64}), owner: owner.Uid, provider: provider, placement: placement, binding: binding, leaf: leaf, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 64)}, nil
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
		select {
		case h.slots <- struct{}{}:
		default:
			//unchecked: best-effort close of a connection rejected for backpressure; this loop has no caller left to report the failure to
			conn.Close()
			continue
		}
		h.workers.Add(1)
		go func() {
			defer h.workers.Done()
			defer func() { <-h.slots }()
			defer conn.Close()
			callCtx, cancel := context.WithTimeout(h.ctx, callBudget)
			defer cancel()
			call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, MaxFrameBytes)
			if call != nil {
				defer call.Close()
			}
			if err == nil {
				base := &receiver{host: h, call: call, ctx: callCtx}
				services := []wire.ServedService{&wire.ChatDispatcher{Handler: base}, &wire.ChatV2Dispatcher{Handler: &chatV2Receiver{base}}, &wire.EmbedderDispatcher{Handler: base}, &wire.PreloaderDispatcher{Handler: base}, &wire.TranscriptionDispatcher{Handler: &transcriptionReceiver{base}}, &wire.SpeechDispatcher{Handler: &speechReceiver{base}}, &wire.ImageDispatcher{Handler: &imageReceiver{base}}, &wire.ImageV2Dispatcher{Handler: &imageV2Receiver{base}}}
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
				var reply []byte
				reply, err = wire.ServeEndpoint(call.Frame, "openabstractions", "", services...)
				if err == nil {
					err = call.Reply(reply)
				}
			}
			if err != nil && h.OnError != nil && h.ctx.Err() == nil {
				h.OnError(err)
			}
		}()
	}
}

// SubjectFromPeer binds account and program from Program proof.
func SubjectFromPeer(peer *identity.Peer) (inference.Subject, error) {
	if peer == nil {
		return inference.Subject{}, errors.New("inference service: native subject evidence required")
	}
	if err := peer.Check(listen.Program); err != nil {
		return inference.Subject{}, err
	}
	u, err := peer.User.AtLeast(listen.Program.User)
	if err != nil {
		return inference.Subject{}, err
	}
	account := ""
	if u.Kind == "windows" {
		account = u.SID
	} else if u.Kind == "posix" && u.UID >= 0 {
		account = strconv.Itoa(u.UID)
	}
	program, err := identity.SubjectProgram(peer, listen.Program.Path)
	if err != nil || account == "" {
		return inference.Subject{}, errors.New("inference service: native subject account/program unavailable")
	}
	return inference.Subject{Account: account, Program: program}, nil
}

type receiver struct {
	host *Host
	call *listen.FramedCall
	ctx  context.Context
}

// admittingContext carries the endpoint's fixed policy and the bound peer
// into admission. Decide reads the peer the subject was bound from
// (inference.PeerFromContext); an unbound caller carries none. A designated
// leaf marks a bound caller as its designated runtime, whose caller claim the
// provider records; every other caller's claim is discarded.
func (r *receiver) admittingContext(subject inference.Subject, rung string, peer *identity.Peer) context.Context {
	ctx := inference.WithExecutionPlacement(inference.WithRoute(r.ctx, inference.RouteNative, rung), r.host.placement)
	ctx = inference.WithPeer(ctx, peer)
	if r.host.binding.Host != "" {
		ctx = inference.WithExecutionBinding(ctx, r.host.binding)
	}
	if r.host.leaf {
		ctx = inference.WithLeafExecution(ctx)
	}
	if len(r.host.designated) > 0 && subject.Program != "" {
		ctx = inference.WithDesignatedRuntime(ctx)
	}
	return ctx
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
	subject, err := SubjectFromPeer(peer)
	if err != nil || subject.Account != r.host.owner || r.call.Recheck() != nil {
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
	return subject, peer.Rung(), peer
}

func (r *receiver) Start(request wire.Request) (wire.Admission, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.Admission{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung, peer := r.bound()
	return r.host.provider.Start(r.admittingContext(subject, rung, peer), subject, request), nil
}

func (r *receiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.Observe(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *receiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.Cancel(r.caller(), operation), nil
}

type chatV2Receiver struct{ *receiver }

func (r *chatV2Receiver) Start(request wire.RequestV2) (wire.AdmissionV2, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.AdmissionV2{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung, peer := r.bound()
	return r.host.provider.StartV2(r.admittingContext(subject, rung, peer), subject, request), nil
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
	return r.host.provider.Preload(r.admittingContext(subject, rung, peer), subject, request), nil
}

func (r *receiver) Embed(request wire.EmbedRequest) (wire.Embeddings, error) {
	subject, rung, peer := r.bound()
	return r.host.provider.Embed(r.admittingContext(subject, rung, peer), subject, request), nil
}

type transcriptionReceiver struct{ *receiver }

func (r *transcriptionReceiver) Start(request wire.TranscriptionRequest) (wire.Admission, error) {
	subject, rung, peer := r.bound()
	return r.host.provider.StartTranscription(r.admittingContext(subject, rung, peer), subject, request), nil
}

type speechReceiver struct{ *receiver }

// videoReceiver hands each video@1 call to the host's VideoServer with the
// bound caller. A retired endpoint admits no new video, as it admits no
// other Start. video@1 is a provider's service for its designated runtime
// (INF-G5): an endpoint that designates none answers every caller
// forbidden.
type videoReceiver struct{ *receiver }

// videoCaller is the bound designated runtime, or the zero Subject.
func (r *videoReceiver) videoCaller() (inference.Subject, string, *identity.Peer) {
	if len(r.host.designated) == 0 {
		return inference.Subject{}, "", nil
	}
	return r.bound()
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
	return r.host.provider.StartImage(r.admittingContext(subject, rung, peer), subject, request), nil
}
func (r *imageReceiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveImage(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *imageReceiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelImage(r.caller(), operation), nil
}

func (r *speechReceiver) Start(request wire.SpeechRequest) (wire.Admission, error) {
	subject, rung, peer := r.bound()
	return r.host.provider.StartSpeech(r.admittingContext(subject, rung, peer), subject, request), nil
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

func (r *selectionOperatorV2Receiver) Read(targetProgram, name string) (wire.SelectionReadV2, error) {
	return r.host.SelectionOperatorV2.ReadSelectionV2(r.ctx, r.caller(), targetProgram, name), nil
}
func (r *selectionOperatorV2Receiver) Put(targetProgram, expectedRevision string, policy wire.SelectionPolicyV2) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.PutSelectionV2(r.ctx, r.caller(), targetProgram, expectedRevision, policy), nil
}
func (r *selectionOperatorV2Receiver) DeletePolicy(targetProgram, expectedRevision, name string) (wire.SelectionChange, error) {
	return r.host.SelectionOperatorV2.DeleteSelectionV2(r.ctx, r.caller(), targetProgram, expectedRevision, name), nil
}

func (r *operatorReceiver) Hosts() (wire.HostList, error) {
	return r.host.Operator.Hosts(r.ctx, r.caller()), nil
}

func (r *operatorReceiver) AddHost(expectedRevision string, host wire.HostEntry) (wire.HostChange, error) {
	return r.host.Operator.AddHost(r.ctx, r.caller(), expectedRevision, host), nil
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
