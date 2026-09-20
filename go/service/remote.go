package service

import (
	"context"
	"errors"

	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
	routerservice "github.com/openabstractions/abstraction-router/go/service"
)

// RouterContract is the router profile a remote runtime serves beside chat@1.
const RouterContract = "abstraction.router/router@1"

// RemoteAuthorize maps an authenticated remote peer to the subject the
// receiving runtime decides for, and names the peer's domain for the audit:
// the account is the namespace its certificate key maps to, and the program
// is the receiving runtime's own executable (research/rights-defaults
// DECISION.md §4). An error refuses the call.
type RemoteAuthorize func(ctx context.Context, peer remote.Peer) (subject inference.Subject, domain string, err error)

// RemoteHandler serves abstraction.inference/chat@1 and router@1 inventory
// to another runtime over the mutual-TLS remote transport. Every call maps its
// peer through authorize before any provider sees it; chat@1 decisions,
// credentials and ceilings are the receiving runtime's own, and each operation
// records the route remote, the peer's domain and the program the delegating
// runtime claims.
func RemoteHandler(provider *inference.Provider, routes *router.Router, authorize RemoteAuthorize) remote.Handler {
	return func(ctx context.Context, peer remote.Peer, frame []byte) ([]byte, error) {
		subject, domain, err := authorize(ctx, peer)
		if err != nil || subject.Account == "" || subject.Program == "" {
			return nil, errors.New("inference remote: peer not mapped")
		}
		r := &remoteChat{provider: provider, subject: subject, ctx: inference.WithRemote(ctx, domain, "tls-remote/"+domain)}
		// The services this runtime serves over the remote transport, as a
		// registry reads them before it offers the remote.
		services := []wire.ServedService{&wire.ChatDispatcher{Handler: r}, &wire.EmbedderDispatcher{Handler: r}, &wire.TranscriptionDispatcher{Handler: &remoteTranscription{r}}, &wire.SpeechDispatcher{Handler: &remoteSpeech{r}}, &wire.ImageDispatcher{Handler: &remoteImage{r}}}
		services = append(services, &wire.LiveDispatcher{Handler: &remoteLive{r}})
		if routes != nil {
			views := routerservice.Remote(routes, listen.Seen{Bound: true, User: domain, Path: "remote:" + domain})
			services = append(services, &routerwire.RouterDispatcher{Handler: views})
		}
		return wire.ServeEndpoint(frame, "openabstractions", "", services...)
	}
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
