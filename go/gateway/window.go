// Package gateway is the loopback window of abstraction.inference/chat@1: an
// OpenAI chat completions and Anthropic messages surface for programs that only
// know a base URL and an API key. It binds every connection's peer through the
// socket-owner table before reading a byte, requires the local key minted for
// that program before reading a request body, and serves each request through
// the same provider, rights decisions and audit as the native route.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// PeerNeed is the least the window asks of a loopback peer: user, process and
// path bound to the process that opened the connection.
var PeerNeed = identity.Need{User: identity.ProofBound, Process: identity.ProofBound, Path: identity.ProofBound}

// MaxBodyBytes bounds one request body, the size of one chat@1 control frame.
const MaxBodyBytes = 1 << 20

// MaxTranscriptionBodyBytes bounds the multipart upload, including framing.
const MaxTranscriptionBodyBytes = (16 << 20) + (1 << 20)
const MaxImageEditBodyBytes = (40 << 20) + (1 << 20)

// Key outcome words a KeyCheck returns.
const (
	KeyVerified     = "verified"
	KeyUnknown      = "unknown"
	KeyRevoked      = "revoked"
	KeyWrongProgram = "wrong_program"
	KeyUnavailable  = "unavailable"
)

// Chat is the provider the window starts, observes and cancels operations on.
// *inference.Provider satisfies it.
type Chat interface {
	Start(ctx context.Context, subject inference.Subject, req wire.Request) wire.Admission
	Observe(ctx context.Context, subject inference.Subject, id string, cursor, maxDeltas, maxBytes, waitMS int64) wire.DeltaPage
	Cancel(subject inference.Subject, id string) wire.Cancellation
}

// Transcriber is the optional transcription profile on the same endpoint.
type Transcriber interface {
	StartTranscription(context.Context, inference.Subject, wire.TranscriptionRequest) wire.Admission
	ObserveTranscription(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage
	CancelTranscription(inference.Subject, string) wire.Cancellation
}

type SpeechSynthesizer interface {
	StartSpeech(context.Context, inference.Subject, wire.SpeechRequest) wire.Admission
	ObserveSpeech(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage
	CancelSpeech(inference.Subject, string) wire.Cancellation
}

type ImageGenerator interface {
	StartImage(context.Context, inference.Subject, wire.ImageRequest) wire.Admission
	ObserveImage(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage
	CancelImage(inference.Subject, string) wire.Cancellation
}

// LiveProvider is the service-owned live voice profile exposed through the
// compatibility window. The browser-facing WebSocket remains a gateway wire;
// provider state and upstream transport stay behind these native calls.
type LiveProvider interface {
	StartLive(context.Context, inference.Subject, wire.LiveRequest) wire.Admission
	AppendLive(context.Context, inference.Subject, string, int64, []byte) wire.LiveInputResult
	ObserveLive(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage
	CommitLive(context.Context, inference.Subject, string) wire.LiveInputResult
	CancelLive(inference.Subject, string) wire.Cancellation
}

// ContentWriter authorizes content.write for subject and stores verified bytes
// under digest. The provider separately authorizes content.read during Start.
type ContentWriter func(context.Context, inference.Subject, string, []byte) inference.ContentOutcome

// ContentReader authorizes content.read for subject and resolves complete bytes.
type ContentReader func(context.Context, inference.Subject, string, int64) ([]byte, inference.ContentOutcome)

// Grant is what a verified local key lets the window ask for: the hosted
// credential name its requests may spend under, or none for local-only.
type Grant struct{ Credential string }

// KeyCheck verifies a presented key for the bound subject and returns a Key*
// outcome word. A key minted for another program is KeyWrongProgram.
type KeyCheck func(ctx context.Context, subject inference.Subject, key string) (Grant, string)

// Bound is a connection's bound peer.
type Bound interface {
	Subject() inference.Subject
	Rung() string
	// Recheck returns an error once the connection no longer answers for the
	// peer it was bound to.
	Recheck() error
	Close() error
}

// Binder binds the peer of an accepted connection. It runs before any byte is
// read.
type Binder func(c net.Conn, acceptedAt time.Time) (Bound, error)

// Config composes a window. Chat, Keys and Account are required.
type Config struct {
	Chat Chat
	Keys KeyCheck
	// Account is the runtime's account; a peer of another account is refused.
	Account string
	// Models lists the model names the window reports for subject.
	Models       func(ctx context.Context, subject inference.Subject) []string
	StoreContent ContentWriter
	ReadContent  ContentReader
	// Bind replaces the socket-owner binding, for tests.
	Bind Binder
	// Record receives one record per window refusal before admission. Admitted
	// and refused requests are recorded by the provider.
	Record  func(inference.Record)
	OnError func(error)
}

// Window is a listening gateway window.
type Window struct {
	cfg      Config
	listener net.Listener
	server   *http.Server
	once     sync.Once
	liveMu   sync.Mutex
	live     map[*liveWindowSession]struct{}
	closed   bool
}

// Listen opens the window on address, which must name 127.0.0.1 and a port.
func Listen(address string, cfg Config) (*Window, error) {
	if cfg.Chat == nil || cfg.Keys == nil || cfg.Account == "" {
		return nil, errors.New("gateway: chat provider, key check and account required")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host != "127.0.0.1" || port == "" {
		return nil, fmt.Errorf("gateway: %q is not 127.0.0.1:<port>; the window listens on IPv4 loopback only", address)
	}
	if cfg.Bind == nil {
		account := cfg.Account
		cfg.Bind = func(c net.Conn, at time.Time) (Bound, error) { return BindLoopback(c, at, account) }
	}
	l, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, err
	}
	w := &Window{cfg: cfg}
	w.listener = &boundListener{Listener: l, window: w}
	w.server = &http.Server{Handler: http.HandlerFunc(w.serveHTTP), ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if b, ok := c.(*boundConn); ok {
				return context.WithValue(ctx, boundKey{}, b.bound)
			}
			return ctx
		}}
	return w, nil
}

// Addr is the listening address.
func (w *Window) Addr() net.Addr { return w.listener.Addr() }

// Serve accepts connections until Close or ctx ends.
func (w *Window) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { w.Close() })
	defer stop()
	err := w.server.Serve(w.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops accepting and closes every connection; admitted operations with
// no observer idle out.
func (w *Window) Close() error {
	var err error
	w.once.Do(func() {
		w.liveMu.Lock()
		w.closed = true
		sessions := make([]*liveWindowSession, 0, len(w.live))
		for session := range w.live {
			sessions = append(sessions, session)
		}
		w.liveMu.Unlock()
		for _, session := range sessions {
			session.stop()
		}
		err = w.server.Close()
	})
	return err
}

func (w *Window) record(r inference.Record) {
	r.Route = inference.RouteWindow
	if w.cfg.Record != nil {
		w.cfg.Record(r)
	}
}

func (w *Window) report(err error) {
	if err != nil && w.cfg.OnError != nil {
		w.cfg.OnError(err)
	}
}

type boundKey struct{}

type boundConn struct {
	net.Conn
	bound Bound
	once  sync.Once
}

func (c *boundConn) Close() error {
	var err error
	c.once.Do(func() { err = errors.Join(c.bound.Close(), c.Conn.Close()) })
	return err
}

// boundListener binds each accepted connection before net/http reads from it.
// A connection whose peer cannot be bound is answered 403 and closed without
// reading its request.
type boundListener struct {
	net.Listener
	window *Window
}

func (l *boundListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		at := time.Now()
		b, err := l.window.cfg.Bind(c, at)
		if err == nil {
			return &boundConn{Conn: c, bound: b}, nil
		}
		word := BindingWord(err)
		l.window.record(inference.Record{Rung: identity.TransportLoopback + "/" + runtime.GOOS + " unbound", Outcome: wire.ReplyOutcomeForbidden.String(), Reason: "binding:" + word})
		refuseConnection(c, word, err)
	}
}

// refuseConnection answers before reading: the peer's request stays unread in
// the socket, and closing discards it.
func refuseConnection(c net.Conn, word string, cause error) {
	body, _ := json.Marshal(errorBody(http.StatusForbidden, "forbidden", "the window could not bind the program on this connection ("+word+"): "+cause.Error()))
	c.SetWriteDeadline(time.Now().Add(time.Second))
	fmt.Fprintf(c, "HTTP/1.1 403 Forbidden\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	c.Close()
}

// BindingWord names a binding failure in an audit reason.
func BindingWord(err error) string {
	switch {
	case errors.Is(err, identity.ErrPeerMoved):
		return "peer_moved"
	case errors.Is(err, identity.ErrPeerGone):
		return "peer_gone"
	case errors.Is(err, identity.ErrRemotePeer):
		return "remote_peer"
	case errors.Is(err, identity.ErrNoBinding):
		return "no_binding"
	case errors.Is(err, identity.ErrNotProven):
		return "not_proven"
	case errors.Is(err, errOtherAccount):
		return "other_account"
	}
	return "unbound"
}

var errOtherAccount = errors.New("gateway: the peer runs as another account")

type loopbackBound struct {
	binding *identity.Binding
	subject inference.Subject
	rung    string
}

func (b *loopbackBound) Subject() inference.Subject { return b.subject }
func (b *loopbackBound) Rung() string               { return b.rung }
func (b *loopbackBound) Close() error               { return b.binding.Close() }
func (b *loopbackBound) Recheck() error {
	_, err := b.binding.Peer()
	return err
}

// BindLoopback binds c's peer through the socket-owner table and requires
// PeerNeed and account.
func BindLoopback(c net.Conn, at time.Time, account string) (Bound, error) {
	b, err := identity.BindLoopback(c, &identity.Options{ConnectedAt: at, SkipCodeSignature: true})
	if err != nil {
		return nil, err
	}
	subject, rung, err := subjectOf(b)
	if err != nil {
		b.Close()
		return nil, err
	}
	if subject.Account != account {
		b.Close()
		return nil, errOtherAccount
	}
	return &loopbackBound{binding: b, subject: subject, rung: rung}, nil
}

func subjectOf(b *identity.Binding) (inference.Subject, string, error) {
	peer, err := b.Peer()
	if err != nil {
		return inference.Subject{}, "", err
	}
	if err := peer.Check(PeerNeed); err != nil {
		return inference.Subject{}, peer.Rung(), err
	}
	u, _ := peer.User.Get()
	account := ""
	switch {
	case u.Kind == "windows":
		account = u.SID
	case u.Kind == "posix" && u.UID >= 0:
		account = strconv.Itoa(u.UID)
	}
	program, err := identity.SubjectProgram(peer, identity.ProofClaimed)
	if account == "" || err != nil {
		return inference.Subject{}, peer.Rung(), errors.New("gateway: the peer's account or program is unavailable")
	}
	return inference.Subject{Account: account, Program: program}, peer.Rung(), nil
}

// wireKind is the vendor wire a request path speaks.
type wireKind int

const (
	wireOpenAI wireKind = iota
	wireAnthropic
)

// presentedKey reads the key an OpenAI client sends as a bearer token or an
// Anthropic client sends as x-api-key.
func presentedKey(r *http.Request) string {
	if key := r.Header.Get("X-Api-Key"); key != "" {
		return key
	}
	if auth := r.Header.Get("Authorization"); len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// embeds reports whether the provider serves embeddings.
func (w *Window) embeds() bool {
	_, ok := w.cfg.Chat.(Embedder)
	return ok
}

func (w *Window) transcribes() bool {
	_, ok := w.cfg.Chat.(Transcriber)
	return ok && w.cfg.StoreContent != nil
}

func (w *Window) speaks() bool { _, ok := w.cfg.Chat.(SpeechSynthesizer); return ok }
func (w *Window) images() bool {
	_, ok := w.cfg.Chat.(ImageGenerator)
	return ok && w.cfg.ReadContent != nil
}
func (w *Window) lives() bool { _, ok := w.cfg.Chat.(LiveProvider); return ok }

type verifiedKeyContext struct{}

// serveHTTP checks the binding, the route and the key before the body is read.
func (w *Window) serveHTTP(rw http.ResponseWriter, r *http.Request) {
	b, _ := r.Context().Value(boundKey{}).(Bound)
	if b == nil {
		writeError(rw, http.StatusForbidden, "forbidden", "unbound connection")
		return
	}
	subject := b.Subject()
	refuse := func(status int, word, reason, message string) {
		w.record(inference.Record{Rung: b.Rung(), Account: subject.Account, Program: subject.Program, Outcome: word, Reason: reason})
		rw.Header().Set("Connection", "close")
		writeError(rw, status, word, message)
	}
	if err := b.Recheck(); err != nil {
		refuse(http.StatusForbidden, "forbidden", "binding:"+BindingWord(err), "the connection no longer answers for the program that opened it: "+err.Error())
		return
	}
	var route func(http.ResponseWriter, *http.Request, Bound, Grant)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		route = w.openAIChat
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		route = w.anthropicMessages
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		route = w.models
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings" && w.embeds():
		route = w.openAIEmbeddings
	case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/transcriptions" && w.transcribes():
		route = w.openAITranscription
	case r.Method == http.MethodPost && r.URL.Path == "/v1/audio/speech" && w.speaks():
		route = w.openAISpeech
	case r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations" && w.images():
		route = w.openAIImageGeneration
	case r.Method == http.MethodPost && r.URL.Path == "/v1/images/edits" && w.images() && w.cfg.StoreContent != nil:
		route = w.openAIImageEdit
	case r.Method == http.MethodGet && r.URL.Path == "/v1/realtime" && w.lives():
		route = w.openAIRealtime
	default:
		rw.Header().Set("Connection", "close")
		writeError(rw, http.StatusNotFound, "not_found", "the window serves POST /v1/chat/completions, POST /v1/messages, POST /v1/embeddings, POST /v1/audio/transcriptions, POST /v1/audio/speech, POST /v1/images/generations, POST /v1/images/edits, GET /v1/realtime and GET /v1/models")
		return
	}
	key := presentedKey(r)
	if key == "" {
		refuse(http.StatusUnauthorized, "forbidden", "key:missing", "a local key is required: openabstractions inference key issue --for <program>")
		return
	}
	grant, word := w.cfg.Keys(r.Context(), subject, key)
	switch word {
	case KeyVerified:
	case KeyWrongProgram:
		refuse(http.StatusForbidden, "forbidden", "key:"+word, "this key was issued to another program")
		return
	case KeyUnavailable:
		refuse(http.StatusServiceUnavailable, "unavailable", "key:"+word, "the key could not be verified")
		return
	default:
		refuse(http.StatusUnauthorized, "forbidden", "key:"+word, "the key is not a local key issued to this program")
		return
	}
	route(rw, r.WithContext(context.WithValue(r.Context(), verifiedKeyContext{}, key)), b, grant)
}

func (w *Window) models(rw http.ResponseWriter, r *http.Request, b Bound, _ Grant) {
	var names []string
	if w.cfg.Models != nil {
		names = w.cfg.Models(r.Context(), b.Subject())
	}
	rw.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Anthropic-Version") != "" {
		data := make([]map[string]any, 0, len(names))
		for _, name := range names {
			data = append(data, map[string]any{"type": "model", "id": name, "display_name": name, "created_at": "1970-01-01T00:00:00Z"})
		}
		out := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
		if len(names) > 0 {
			out["first_id"], out["last_id"] = names[0], names[len(names)-1]
		}
		json.NewEncoder(rw).Encode(out)
		return
	}
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "openabstractions"})
	}
	json.NewEncoder(rw).Encode(map[string]any{"object": "list", "data": data})
}

// readBody reads a bounded request body after the key was verified, and
// rechecks the binding before anything is admitted.
func (w *Window) readBody(rw http.ResponseWriter, r *http.Request, b Bound, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(rw, r.Body, MaxBodyBytes))
	if err := dec.Decode(into); err != nil {
		writeError(rw, http.StatusBadRequest, "invalid", "the request body is not a JSON object within 1 MiB: "+err.Error())
		return false
	}
	if err := b.Recheck(); err != nil {
		subject := b.Subject()
		w.record(inference.Record{Rung: b.Rung(), Account: subject.Account, Program: subject.Program, Outcome: "forbidden", Reason: "binding:" + BindingWord(err)})
		rw.Header().Set("Connection", "close")
		writeError(rw, http.StatusForbidden, "forbidden", "the connection no longer answers for the program that opened it: "+err.Error())
		return false
	}
	return true
}

// start admits request for the bound subject under the key's grant.
func (w *Window) start(r *http.Request, b Bound, grant Grant, req wire.Request) wire.Admission {
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	ctx := inference.WithRoute(r.Context(), inference.RouteWindow, b.Rung())
	return w.cfg.Chat.Start(ctx, b.Subject(), req)
}

// observe yields each page of an admitted operation until its end. It cancels
// the operation when the client leaves or emit fails, and returns the reply.
func (w *Window) observe(r *http.Request, b Bound, operation string, emit func(wire.Delta) bool) (*wire.Reply, bool) {
	subject := b.Subject()
	cursor := int64(0)
	for {
		page := w.cfg.Chat.Observe(r.Context(), subject, operation, cursor, 256, 65536, 25000)
		if r.Context().Err() != nil {
			w.cfg.Chat.Cancel(subject, operation)
			return nil, false
		}
		if page.Outcome != wire.PageOutcomePage {
			w.report(fmt.Errorf("gateway: observe %s: %s", operation, page.Outcome))
			w.cfg.Chat.Cancel(subject, operation)
			return &wire.Reply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "observe:" + page.Outcome.String()}, true
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindEnd && d.End != nil {
				if !emit(d) {
					return nil, false
				}
				return d.End, true
			}
			if !emit(d) {
				w.cfg.Chat.Cancel(subject, operation)
				return nil, false
			}
		}
		cursor = page.Next
	}
}

// status is the HTTP status of a refusal or failed reply outcome.
func status(outcome string) int {
	switch outcome {
	case "invalid", "unsupported_feature":
		return http.StatusBadRequest
	case "not_permitted", "forbidden":
		return http.StatusForbidden
	case "no_host":
		return http.StatusNotFound
	case "budget_exceeded", "exhausted":
		return http.StatusTooManyRequests
	case "refused":
		return http.StatusBadGateway
	}
	return http.StatusServiceUnavailable
}

func errorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	return "api_error"
}

// errorBody is readable by both wires: Anthropic reads type and error.type,
// OpenAI reads error.message and error.code.
func errorBody(status int, code, message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]any{"type": errorType(status), "message": message, "code": code}}
}

func writeError(rw http.ResponseWriter, status int, code, message string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(errorBody(status, code, message))
}

func refusalMessage(outcome, reason string) string {
	if reason == "" {
		return outcome
	}
	return outcome + ": " + reason
}
