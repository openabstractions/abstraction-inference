// Package service hosts abstraction.inference/chat@1, and operator@1 when an
// operator is composed, on one shared framed IPC endpoint, binding every caller
// by native Program proof.
package service

import (
	"context"
	"errors"
	"os/user"
	"runtime"
	"strconv"
	"sync"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// MaxFrameBytes bounds one request frame; a Request fits one control frame.
const MaxFrameBytes = 1 << 20

// callBudget covers the longest Observe wait and its reply.
const callBudget = 35 * time.Second

// OperatorContract is the wire contract of the operator profile.
const OperatorContract = "abstraction.inference/operator@1"

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
	ctx       context.Context
	cancel    context.CancelFunc
	once      sync.Once
	workers   sync.WaitGroup
	slots     chan struct{}
	OnError   func(error)
	// Assign before Serve. Called when admission stops, before calls drain.
	OnStopped func()
	// Operator, assigned before Serve, serves operator@1 on the same endpoint.
	// Nil refuses that profile.
	Operator Operator
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
	l, err := listen.Listen(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{listener: l, owner: owner.Uid, provider: provider, placement: placement, binding: binding, leaf: leaf, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 64)}, nil
}

func (h *Host) Close() error {
	var err error
	h.once.Do(func() { h.cancel(); err = h.listener.Close() })
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
				services := []wire.ServedService{&wire.ChatDispatcher{Handler: base}, &wire.EmbedderDispatcher{Handler: base}, &wire.TranscriptionDispatcher{Handler: &transcriptionReceiver{base}}, &wire.SpeechDispatcher{Handler: &speechReceiver{base}}, &wire.ImageDispatcher{Handler: &imageReceiver{base}}}
				services = append(services, &wire.LiveDispatcher{Handler: &liveReceiver{base}})
				if h.Operator != nil {
					services = append(services, &wire.OperatorDispatcher{Handler: &operatorReceiver{base}})
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

func (r *receiver) admittingContext(rung string) context.Context {
	ctx := inference.WithExecutionPlacement(inference.WithRoute(r.ctx, inference.RouteNative, rung), r.host.placement)
	if r.host.binding.Host != "" {
		ctx = inference.WithExecutionBinding(ctx, r.host.binding)
	}
	if r.host.leaf {
		ctx = inference.WithLeafExecution(ctx)
	}
	return ctx
}

// caller is the bound subject of this runtime's account, or the zero Subject,
// which the provider refuses as forbidden, and the rung it was bound at. macOS
// has no Program proof.
func (r *receiver) caller() inference.Subject {
	subject, _ := r.bound()
	return subject
}

func (r *receiver) bound() (inference.Subject, string) {
	if runtime.GOOS == "darwin" {
		return inference.Subject{}, ""
	}
	peer, err := r.call.Peer()
	if err != nil {
		return inference.Subject{}, ""
	}
	subject, err := SubjectFromPeer(peer)
	if err != nil || subject.Account != r.host.owner || r.call.Recheck() != nil {
		return inference.Subject{}, peer.Rung()
	}
	return subject, peer.Rung()
}

func (r *receiver) Start(request wire.Request) (wire.Admission, error) {
	r.host.admission.RLock()
	if r.host.retired {
		r.host.admission.RUnlock()
		return wire.Admission{Outcome: wire.StartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	defer r.host.admission.RUnlock()
	subject, rung := r.bound()
	return r.host.provider.Start(r.admittingContext(rung), subject, request), nil
}

func (r *receiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.Observe(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *receiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.Cancel(r.caller(), operation), nil
}

func (r *receiver) Embed(request wire.EmbedRequest) (wire.Embeddings, error) {
	subject, rung := r.bound()
	return r.host.provider.Embed(r.admittingContext(rung), subject, request), nil
}

type transcriptionReceiver struct{ *receiver }

func (r *transcriptionReceiver) Start(request wire.TranscriptionRequest) (wire.Admission, error) {
	subject, rung := r.bound()
	return r.host.provider.StartTranscription(r.admittingContext(rung), subject, request), nil
}

type speechReceiver struct{ *receiver }

type imageReceiver struct{ *receiver }

func (r *imageReceiver) Start(request wire.ImageRequest) (wire.Admission, error) {
	subject, rung := r.bound()
	return r.host.provider.StartImage(r.admittingContext(rung), subject, request), nil
}
func (r *imageReceiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveImage(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *imageReceiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelImage(r.caller(), operation), nil
}

func (r *speechReceiver) Start(request wire.SpeechRequest) (wire.Admission, error) {
	subject, rung := r.bound()
	return r.host.provider.StartSpeech(r.admittingContext(rung), subject, request), nil
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
