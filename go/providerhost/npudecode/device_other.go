//go:build !windows

package npudecode

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelpack"
)

// errPlatform: the NPU decoder is built for Windows' XRT only.
var errPlatform = fmt.Errorf("npudecode: the NPU decoder runs on Windows only: %w", errors.ErrUnsupported)

// Device is the NPU decoder; on this platform none opens.
type Device struct{}

// Query reports errors.ErrUnsupported on this platform.
func Query(Config) (string, error) { return "", errPlatform }

// Open reports errors.ErrUnsupported on this platform.
func Open(context.Context, Config) (*Device, error) { return nil, errPlatform }

func (*Device) Info() string                      { return "" }
func (*Device) Shape() Shape                      { return Shape{} }
func (*Device) Stats() (int, time.Duration)       { return 0, 0 }
func (*Device) Slots() int                        { return 0 }
func (*Device) Sets() int                         { return 0 }
func (*Device) In(int) []byte                     { return nil }
func (*Device) Out(int) []byte                    { return nil }
func (*Device) TryAcquire() bool                  { return false }
func (*Device) Done()                             {}
func (*Device) Err() error                        { return errPlatform }
func (*Device) Distrust(error)                    {}
func (*Device) Decode(context.Context, int) error { return errPlatform }
func (*Device) Close() error                      { return nil }

var _ modelpack.FixedDevice = (*Device)(nil)
