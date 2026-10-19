// Package providerhost composes a native inference provider from the existing
// router, inference lifecycle, authenticated leaf endpoint and optional content
// component. Engines implement inference.ImageEngine, TranscriptionEngine or
// SpeechEngine, or an existing leaf wire adapter for image work; this package
// does not define a second provider protocol. NewImage serves image@1;
// NewEngine serves a managed engine's list of profiles (image, transcription,
// speech) over the same native transport.
package providerhost

import (
	"context"
	"errors"
	"sync"

	inference "github.com/openabstractions/abstraction-inference/go"
	"github.com/openabstractions/abstraction-inference/go/service"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

// Config names one native endpoint and the runtime executables allowed to
// forward authorized requests. Inference.Config supplies the explicit router,
// rights and content callbacks. Content's policies and store are separate and
// mandatory when the component is attached.
type Config struct {
	Endpoint        string
	RuntimePrograms []string
	Inference       inference.Config
	Content         *storageService.Component
	// AuditPath is the absolute file of the provider's persistent audit
	// journal. Each provider record is appended there before
	// Inference.Record receives it. Program is the designated runtime and
	// Claim the original caller it named. Empty keeps no journal.
	AuditPath string
}

// Host owns a provider and its designated native leaf endpoint. Its composed
// storage component is closed with the endpoint.
type Host struct {
	Service  *service.Host
	Provider *inference.Provider
	// Audit is the persistent provider journal, or nil without AuditPath.
	Audit *inference.Journal
	once  sync.Once
}

// New creates a local leaf. A callback engine is bound to the exact router
// declaration supplied in Inference.ImageEngineHost; the inference provider
// rechecks this identity at image admission and execution.
func New(cfg Config) (*Host, error) {
	if cfg.Endpoint == "" || len(cfg.RuntimePrograms) == 0 {
		return nil, errors.New("providerhost: endpoint and designated runtime required")
	}
	if cfg.Inference.ImageEngine != nil && cfg.Inference.ImageEngineHost == nil {
		return nil, errors.New("providerhost: image engine host binding required")
	}
	if cfg.Inference.TranscriptionEngine != nil && cfg.Inference.TranscriptionEngineHost == nil || cfg.Inference.SpeechEngine != nil && cfg.Inference.SpeechEngineHost == nil {
		return nil, errors.New("providerhost: audio engine host binding required")
	}
	var journal *inference.Journal
	if cfg.AuditPath != "" {
		var err error
		if journal, err = inference.OpenJournal(cfg.AuditPath, 0, nil); err != nil {
			return nil, err
		}
		next, report := cfg.Inference.Record, cfg.Inference.OnError
		cfg.Inference.Record = func(r inference.Record) {
			if err := journal.Append(r); err != nil && report != nil {
				report(err)
			}
			if next != nil {
				next(r)
			}
		}
	}
	p, err := inference.New(cfg.Inference)
	if err != nil {
		return nil, err
	}
	h, err := service.ListenDesignatedLeaf(cfg.Endpoint, p, inference.ExecutionLocal, cfg.RuntimePrograms...)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	h.Content = cfg.Content
	return &Host{Service: h, Provider: p, Audit: journal}, nil
}

// Serve runs the existing authenticated endpoint and closes both lifetimes on
// exit. The caller supplies readiness and any owned engine process lifecycle.
func (h *Host) Serve(ctx context.Context) error {
	defer h.Close()
	return h.Service.Serve(ctx)
}

func (h *Host) Close() error {
	var err error
	h.once.Do(func() {
		err = h.Service.Close()
		if providerErr := h.Provider.Close(); err == nil {
			err = providerErr
		}
	})
	return err
}
