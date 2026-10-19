package providerhost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	casapi "github.com/openabstractions/abstraction-cas/go/api"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

// ImageConfig is the standard local image-provider setup. Host declares one
// provider-local image engine, such as router.NewImageEngine or an explicitly
// configured OpenAI-compatible HTTP adapter. Engine is required for the
// former; HTTP is required for the latter and should verify the owned server
// process before sending requests. Admit may start/load the engine on demand.
// RuntimePrograms names the OA runtimes allowed to forward authorized calls.
type ImageConfig struct {
	Endpoint        string
	RuntimePrograms []string
	Host            *router.Host
	Engine          inference.ImageEngine
	HTTP            *http.Client
	Admit           inference.Admitter
	StoreRoot       string
	RecordPath      string
	// MaxImageBytes bounds each edit input image (inference
	// Config.MaxImageBytes); zero selects inference.DefaultMaxImageBytes. A
	// guarded HTTP client that reads request bodies bounds them by the same
	// value.
	MaxImageBytes int64
	// ImageFields maps the image extensions Host declares to request fields
	// of an OpenAI-compatible Host (inference.Config.ImageFields).
	ImageFields func(extensions map[string]string) (map[string]any, string)
	// AuditPath is the provider's persistent audit journal (Config.AuditPath).
	// Empty selects DefaultAuditPath(RecordPath).
	AuditPath string
	// Record receives each provider record after the journal keeps it.
	// Program is the designated runtime and Claim the original caller it named.
	Record func(inference.Record)
	// OnError receives provider faults, including a failed journal append.
	OnError func(error)
}

// EngineConfig is a managed engine provider's setup: one local host that
// serves the profiles listed in Profiles, each of image, transcription and
// speech. Host.Profiles must name exactly those profiles. Image work runs
// through ImageEngine or HTTP as NewImage's does. Transcription and speech
// run only through the explicitly composed TranscriptionEngine and
// SpeechEngine, which build the engine's requests themselves; the generic
// HTTP adapters never reach a managed engine's audio routes. Every other
// field is ImageConfig's.
type EngineConfig struct {
	Endpoint            string
	RuntimePrograms     []string
	Host                *router.Host
	Profiles            []string
	ImageEngine         inference.ImageEngine
	TranscriptionEngine inference.TranscriptionEngine
	SpeechEngine        inference.SpeechEngine
	HTTP                *http.Client
	Admit               inference.Admitter
	StoreRoot           string
	RecordPath          string
	AuditPath           string
	MaxImageBytes       int64
	// ImageFields is ImageConfig's: the image extensions Host declares,
	// mapped to request fields of an OpenAI-compatible Host.
	ImageFields func(extensions map[string]string) (map[string]any, string)
	Record      func(inference.Record)
	OnError     func(error)
}

// DefaultAuditPath is the audit journal beside a provider's content record
// file: the record path without its extension, followed by "-audit.jsonl".
func DefaultAuditPath(recordPath string) string {
	return strings.TrimSuffix(recordPath, filepath.Ext(recordPath)) + "-audit.jsonl"
}

const imageLimit = 20 << 20

// Audio bounds: transcription input (INF-M5) and one speech delivery
// (inference's MaxSpeechBytes default).
const (
	audioLimit  = 16 << 20
	speechLimit = 64 << 20
)

// DesignatedContent is a provider's content store served on its own
// endpoint: content-reader@1 and content-writer@1 answer only the
// designated runtimes and this provider's own executable. The runtime stages
// inputs through the writer and reads deliveries through the reader.
type DesignatedContent struct {
	Store     *storage.ContentStore
	Component *storageService.Component
	// Runtime reports whether program is one of the designated runtimes.
	Runtime func(program string) bool
	// Self is this provider's own executable, canonical.
	Self string
}

// NewDesignatedContent makes the store under storeRoot, named name, and its
// component with the writer enabled for uploads of at most limit bytes,
// keeping upload records at recordPath. Assign Component to the endpoint's
// service host; closing the host closes it. Every provider this package
// composes, and the model host's video kind, take their content store here.
func NewDesignatedContent(name, storeRoot, recordPath string, runtimePrograms []string, limit int64) (*DesignatedContent, error) {
	if name == "" || len(runtimePrograms) == 0 || !filepath.IsAbs(storeRoot) || !filepath.IsAbs(recordPath) || limit < 1 {
		return nil, errors.New("providerhost: store name, runtime, store root, absolute record path and limit required")
	}
	for _, program := range runtimePrograms {
		if !filepath.IsAbs(program) || !identity.ValidSubjectProgram(program) {
			return nil, errors.New("providerhost: runtime executable must be absolute")
		}
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	self = identity.CanonicalProgramPath(filepath.Clean(self))
	allowed := make([]string, 0, len(runtimePrograms))
	for _, program := range runtimePrograms {
		allowed = append(allowed, identity.CanonicalProgramPath(program))
	}
	runtimeAllowed := func(program string) bool {
		for _, expected := range allowed {
			if identity.SameSubjectProgram(program, expected) {
				return true
			}
		}
		return false
	}
	contentAllowed := func(ctx context.Context, peer *identity.Peer, _ string) error {
		if ctx.Err() != nil || peer == nil || peer.Check(listen.Program) != nil {
			return errors.New("providerhost: content caller unproven")
		}
		program, err := identity.SubjectProgram(peer, listen.Program.Path)
		if err != nil || !runtimeAllowed(program) && !identity.SameSubjectProgram(program, self) {
			return errors.New("providerhost: content caller not designated")
		}
		return nil
	}
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		return nil, err
	}
	store, err := storage.NewContentStore(name, storeRoot)
	if err != nil {
		return nil, err
	}
	component, err := storageService.NewComponent(store, contentAllowed)
	if err != nil {
		return nil, err
	}
	if err := component.EnableWriter(contentAllowed, limit, casapi.BoundedFileStore{MaxBytes: storageService.MaxRecordFileBytes}, recordPath); err != nil {
		_ = component.Close()
		return nil, err
	}
	return &DesignatedContent{Store: store, Component: component, Runtime: runtimeAllowed, Self: self}, nil
}

// managedProfiles are the profiles a managed engine provider can serve.
var managedProfiles = map[string]bool{router.ProfileImage: true, router.ProfileTranscription: true, router.ProfileSpeech: true}

// NewImage builds a leaf provider for designated OA runtimes, not an
// application-facing content resolver. Its image deliveries are local only to
// this provider store. The runtime's native image mediator must fetch them
// from this bound endpoint and publish verified bytes into its own
// caller-authorized store before returning a delivery to an application.
// Content reader/writer share the authenticated image endpoint. Only the
// designated runtime and this provider executable can use those services;
// the provider uses the writer for output publication. Custom policies can
// use New with an explicit inference.Config and storage.Component instead.
func NewImage(cfg ImageConfig) (*Host, error) {
	if cfg.Host == nil || cfg.Host.Hosted || cfg.Host.Wire == router.WireNative || cfg.Host.Wire == router.WireRemote || cfg.Host.ImageURL() == "" {
		return nil, errors.New("providerhost: one local image host required")
	}
	return newEngine(EngineConfig{Endpoint: cfg.Endpoint, RuntimePrograms: cfg.RuntimePrograms, Host: cfg.Host, Profiles: []string{router.ProfileImage},
		ImageEngine: cfg.Engine, HTTP: cfg.HTTP, Admit: cfg.Admit, StoreRoot: cfg.StoreRoot, RecordPath: cfg.RecordPath, AuditPath: cfg.AuditPath,
		MaxImageBytes: cfg.MaxImageBytes, ImageFields: cfg.ImageFields, Record: cfg.Record, OnError: cfg.OnError}, false)
}

// NewEngine builds a leaf provider, as NewImage does, for a managed engine
// that serves a list of profiles. Speech deliveries are local to this
// provider store like image deliveries; transcription input reaches it
// through the same content writer, staged by the designated runtime under
// the original caller's content policy.
func NewEngine(cfg EngineConfig) (*Host, error) { return newEngine(cfg, true) }

// newEngine builds the provider. exact requires the host to declare exactly
// cfg.Profiles; NewImage, which predates the profile list, only requires the
// image profile.
func newEngine(cfg EngineConfig, exact bool) (*Host, error) {
	if cfg.Host == nil || cfg.Host.Hosted || cfg.Host.Wire == router.WireNative || cfg.Host.Wire == router.WireRemote {
		return nil, errors.New("providerhost: one local engine host required")
	}
	if len(cfg.Profiles) == 0 {
		return nil, errors.New("providerhost: profile list required")
	}
	declared := slices.Clone(cfg.Host.HostProfiles())
	slices.Sort(declared)
	listed := slices.Clone(cfg.Profiles)
	slices.Sort(listed)
	for i, profile := range listed {
		if !managedProfiles[profile] || i > 0 && listed[i-1] == profile {
			return nil, errors.New("providerhost: profiles are image, transcription and speech, each once")
		}
	}
	if exact && !slices.Equal(declared, listed) {
		return nil, errors.New("providerhost: the host must declare exactly the provider's profiles")
	}
	serves := func(profile string) bool { return slices.Contains(listed, profile) }
	if serves(router.ProfileImage) {
		if cfg.Host.ImageURL() == "" {
			return nil, errors.New("providerhost: one local image host required")
		}
		if cfg.Host.Wire == router.WireImageEngine && cfg.ImageEngine == nil || cfg.Host.Wire != router.WireImageEngine && cfg.HTTP == nil {
			return nil, errors.New("providerhost: explicit image engine or guarded HTTP client required")
		}
	} else if cfg.ImageEngine != nil {
		return nil, errors.New("providerhost: image engine without the image profile")
	}
	if serves(router.ProfileTranscription) != (cfg.TranscriptionEngine != nil) {
		return nil, errors.New("providerhost: the transcription profile and an explicit transcription engine go together")
	}
	if serves(router.ProfileSpeech) != (cfg.SpeechEngine != nil) {
		return nil, errors.New("providerhost: the speech profile and an explicit speech engine go together")
	}
	if len(cfg.RuntimePrograms) == 0 || cfg.Endpoint == "" || !filepath.IsAbs(cfg.StoreRoot) || !filepath.IsAbs(cfg.RecordPath) {
		return nil, errors.New("providerhost: endpoint, runtime, store root and absolute record path required")
	}
	for _, program := range cfg.RuntimePrograms {
		if !filepath.IsAbs(program) || !identity.ValidSubjectProgram(program) {
			return nil, errors.New("providerhost: runtime executable must be absolute")
		}
	}
	// The content writer takes the largest object the declared profiles
	// stage or publish: image inputs and outputs, transcription input,
	// speech output.
	writerLimit := int64(0)
	if serves(router.ProfileImage) {
		writerLimit = max(writerLimit, imageLimit)
	}
	if serves(router.ProfileTranscription) {
		writerLimit = max(writerLimit, audioLimit)
	}
	if serves(router.ProfileSpeech) {
		writerLimit = max(writerLimit, speechLimit)
	}
	content, err := NewDesignatedContent(cfg.Host.Name, cfg.StoreRoot, cfg.RecordPath, cfg.RuntimePrograms, writerLimit)
	if err != nil {
		return nil, err
	}
	store, component, runtimeAllowed := content.Store, content.Component, content.Runtime
	// outputLimit is the publication bound of one profile's generated output.
	outputLimit := func(profile, media string) (int64, bool) {
		switch {
		case profile == "image" && serves(router.ProfileImage) && media == "image/*":
			return imageLimit, true
		case profile == "speech" && serves(router.ProfileSpeech) && strings.HasPrefix(media, "audio/"):
			return speechLimit, true
		}
		return 0, false
	}
	prepare, err := content.OutputWriter(cfg.Endpoint, outputLimit)
	if err != nil {
		_ = component.Close()
		return nil, err
	}
	routes := router.New(cfg.Host)
	providerCfg := inference.Config{Router: routes, HTTP: cfg.HTTP, Admit: cfg.Admit, Record: cfg.Record, OnError: cfg.OnError,
		MaxImageBytes: cfg.MaxImageBytes, MaxAudioBytes: audioLimit, MaxSpeechBytes: speechLimit,
		ImageFields: cfg.ImageFields, ImageFieldsHost: cfg.Host,
		Decide: func(ctx context.Context, subject inference.Subject, action, resource string) (string, error) {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if runtimeAllowed(subject.Program) && action == inference.ActionComplete && resource == inference.ResourceHost(cfg.Host.Name) {
				return "permitted", nil
			}
			return "not_granted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveContent: func(ctx context.Context, subject inference.Subject, digest string, max int64) ([]byte, inference.ContentOutcome) {
			if ctx.Err() != nil || !runtimeAllowed(subject.Program) || max < 1 {
				return nil, inference.ContentForbidden
			}
			ref, ok := store.Find(digest)
			if !ok || ref.Size < 1 || ref.Size > max {
				return nil, inference.ContentUnavailable
			}
			file, err := os.Open(store.Path(ref))
			if err != nil {
				return nil, inference.ContentUnavailable
			}
			defer file.Close()
			data, err := io.ReadAll(io.LimitReader(file, max+1))
			if err != nil || int64(len(data)) != ref.Size || int64(len(data)) > max {
				return nil, inference.ContentUnavailable
			}
			sum := sha256.Sum256(data)
			if digest != "sha256:"+hex.EncodeToString(sum[:]) {
				return nil, inference.ContentUnavailable
			}
			return data, inference.ContentResolved
		},
		PrepareContentWrite: prepare,
	}
	if serves(router.ProfileImage) {
		providerCfg.ImageEngine, providerCfg.ImageEngineHost = cfg.ImageEngine, cfg.Host
	}
	if serves(router.ProfileTranscription) {
		providerCfg.TranscriptionEngine, providerCfg.TranscriptionEngineHost = cfg.TranscriptionEngine, cfg.Host
	}
	if serves(router.ProfileSpeech) {
		providerCfg.SpeechEngine, providerCfg.SpeechEngineHost = cfg.SpeechEngine, cfg.Host
	}
	audit := cfg.AuditPath
	if audit == "" {
		audit = DefaultAuditPath(cfg.RecordPath)
	}
	h, err := New(Config{Endpoint: cfg.Endpoint, RuntimePrograms: cfg.RuntimePrograms, Inference: providerCfg, Content: component, AuditPath: audit})
	if err != nil {
		_ = component.Close()
		return nil, err
	}
	return h, nil
}

// OutputWriter is the provider's preparer of generated-output commits for
// this store (inference Config.PrepareContentWrite): limit gives a
// profile's output bound for a media type, and refuses one the provider
// does not publish. Each commit is uploaded under a single-use request id
// through this store's content writer on endpoint, the provider's own,
// which only this executable reaches as a server. Only a designated
// runtime's subject is served. An image's media type is the one its bytes
// sniff as, and must be an image type; any other output must match its
// declared type. Every provider this package composes and the model host
// publish their outputs through it.
func (c *DesignatedContent) OutputWriter(endpoint string, limit func(profile, media string) (int64, bool)) (inference.ContentWritePreparer, error) {
	if endpoint == "" || limit == nil {
		return nil, errors.New("providerhost: an endpoint and an output bound are required")
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	writer := listen.FrameClient{Endpoint: endpoint, Server: &listen.ServerExpectation{Principal: principal, Program: c.Self}, Timeout: 35 * time.Second, MaxFrame: 1 << 20, Sessions: true}
	return func(ctx context.Context, subject inference.Subject, profile, media string, max int64) (inference.ContentCommitter, inference.ContentOutcome) {
		bound, ok := limit(profile, media)
		if ctx.Err() != nil || !c.Runtime(subject.Program) || !ok || max < 1 || max > bound {
			return nil, inference.ContentForbidden
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, inference.ContentUnavailable
		}
		requestID := "provider-output-" + hex.EncodeToString(nonce[:])
		return func(ctx context.Context, digest string, data []byte) inference.ContentOutcome {
			if ctx.Err() != nil || len(data) == 0 || int64(len(data)) > bound {
				return inference.ContentUnavailable
			}
			published := media
			if profile == "image" {
				published = http.DetectContentType(data)
				if !mediationcontent.ImageMedia(published) {
					return inference.ContentUnavailable
				}
			} else if !mediationcontent.MediaMatches(data, published) {
				return inference.ContentUnavailable
			}
			if err := mediationcontent.Upload(ctx, writer, requestID, digest, published, data, bound); err != nil {
				return inference.ContentUnavailable
			}
			return inference.ContentResolved
		}, inference.ContentResolved
	}, nil
}
