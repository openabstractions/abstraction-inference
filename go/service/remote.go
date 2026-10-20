package service

import (
	"context"
	"errors"

	"github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
	routerservice "github.com/openabstractions/abstraction-router/go/service"
)

// RouterContract is the router profile a remote runtime serves beside chat@1.
const RouterContract = "abstraction.router/router@1"

// RemoteConfig holds explicit receiving configuration. Account names the local
// policy owner; Domain supplies a display label and grants no authority.
type RemoteConfig struct {
	Account      string
	Admit        func(remote.Peer) error
	Domain       func(context.Context, remote.Peer) (string, error)
	RouterPolicy routerservice.RemotePolicy
}

// RemoteHandler serves abstraction.inference/chat@1 and router@1 inventory
// to another runtime over the mutual-TLS remote transport. Every call maps its
// peer through the shared pinned-key mapping before any provider sees it; chat@1 decisions,
// credentials and ceilings are the receiving runtime's own, and each operation
// records the route remote, the peer's domain and the program the delegating
// runtime claims.
func RemoteHandler(provider *inference.Provider, routes *router.Router, config RemoteConfig) remote.Handler {
	return func(ctx context.Context, peer remote.Peer, frame []byte) ([]byte, error) {
		r, selected, err := remoteServices(ctx, peer, provider, routes, config)
		if err != nil {
			return nil, err
		}
		// Preserve the legacy modality set and its Describe order.
		services := append([]wire.ServedService{}, selected[:2]...)
		services = append(services, &wire.TranscriptionDispatcher{Handler: &remoteTranscription{r}}, &wire.SpeechDispatcher{Handler: &remoteSpeech{r}}, &wire.ImageDispatcher{Handler: &remoteImage{r}})
		services = append(services, &wire.LiveDispatcher{Handler: &remoteLive{r}})
		services = append(services, selected[2:]...)
		return wire.ServeEndpoint(frame, "openabstractions", "", services...)
	}
}

// RemoteServices constructs the existing chat, embed and optional router
// receivers for one admitted call. The composing host owns framing, explicit
// offered versions and profile admission. It must mark incoming execution leaf.
func RemoteServices(ctx context.Context, peer remote.Peer, provider *inference.Provider, routes *router.Router, config RemoteConfig) ([]wire.ServedService, error) {
	_, services, err := remoteServices(ctx, peer, provider, routes, config)
	return services, err
}

func remoteServices(ctx context.Context, peer remote.Peer, provider *inference.Provider, routes *router.Router, config RemoteConfig) (*remoteChat, []wire.ServedService, error) {
	if config.Account == "" || config.Admit == nil || ctx.Err() != nil {
		return nil, nil, errors.New("inference remote: receiving account and admission required")
	}
	mapped, err := remote.SubjectOf(peer, config.Admit)
	if err != nil {
		return nil, nil, errors.New("inference remote: peer not mapped")
	}
	domain := mapped.Program()
	if config.Domain != nil {
		domain, err = config.Domain(ctx, peer)
		if err != nil {
			return nil, nil, err
		}
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	subject := inference.Subject{Account: config.Account, Program: mapped.Program()}
	r := &remoteChat{provider: provider, subject: subject, ctx: inference.WithPeer(inference.WithRemote(ctx, domain, "tls-remote/"+domain), nil)}
	services := []wire.ServedService{&wire.ChatDispatcher{Handler: r}, &wire.EmbedderDispatcher{Handler: r}}
	if routes != nil && config.RouterPolicy != nil {
		views := routerservice.Remote(ctx, routes, routerservice.RemoteCaller{Peer: mapped, Domain: domain}, config.RouterPolicy)
		services = append(services, &routerwire.RouterDispatcher{Handler: views})
	}
	return r, services, nil
}

type remoteChat struct {
	provider *inference.Provider
	subject  inference.Subject
	ctx      context.Context
}

func (r *remoteChat) Start(request wire.Request) (wire.Admission, error) {
	return r.provider.Start(r.ctx, r.subject, request), nil
}

func (r *remoteChat) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.provider.Observe(r.ctx, r.subject, operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *remoteChat) Cancel(operation string) (wire.Cancellation, error) {
	return r.provider.Cancel(r.subject, operation), nil
}

func (r *remoteChat) Embed(request wire.EmbedRequest) (wire.Embeddings, error) {
	return r.provider.Embed(r.ctx, r.subject, request), nil
}

type remoteTranscription struct{ *remoteChat }

func (r *remoteTranscription) Start(request wire.TranscriptionRequest) (wire.Admission, error) {
	return r.provider.StartTranscription(r.ctx, r.subject, request), nil
}

type remoteSpeech struct{ *remoteChat }

type remoteImage struct{ *remoteChat }

func (r *remoteImage) Start(request wire.ImageRequest) (wire.Admission, error) {
	return r.provider.StartImage(r.ctx, r.subject, request), nil
}
func (r *remoteImage) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.provider.ObserveImage(r.ctx, r.subject, operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *remoteImage) Cancel(operation string) (wire.Cancellation, error) {
	return r.provider.CancelImage(r.subject, operation), nil
}

func (r *remoteSpeech) Start(request wire.SpeechRequest) (wire.Admission, error) {
	return r.provider.StartSpeech(r.ctx, r.subject, request), nil
}
func (r *remoteSpeech) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.provider.ObserveSpeech(r.ctx, r.subject, operation, cursor, maxDeltas, maxBytes, waitMS), nil
}
func (r *remoteSpeech) Cancel(operation string) (wire.Cancellation, error) {
	return r.provider.CancelSpeech(r.subject, operation), nil
}

func (r *remoteTranscription) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.provider.ObserveTranscription(r.ctx, r.subject, operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *remoteTranscription) Cancel(operation string) (wire.Cancellation, error) {
	return r.provider.CancelTranscription(r.subject, operation), nil
}
