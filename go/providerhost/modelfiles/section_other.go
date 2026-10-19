//go:build !windows

package modelfiles

import (
	"context"
	"errors"
	"fmt"
)

// Section is unbuilt outside Windows (INF-MS6).
type Section struct{}

// Fill answers errors.ErrUnsupported. release, when not nil, is called before
// it returns, as for every refused fill.
func Fill(_ context.Context, _ Extent, _ string, release func()) (*Section, error) {
	if release != nil {
		release()
	}
	return nil, fmt.Errorf("modelfiles: memory sections are built on Windows only: %w", errors.ErrUnsupported)
}

// RegionReader is unbuilt outside Windows.
type RegionReader struct{}

// OpenRegion answers errors.ErrUnsupported.
func OpenRegion(Extent) (*RegionReader, error) { return nil, errors.ErrUnsupported }

func (*RegionReader) ReadAt([]byte, int64) (int, error) { return 0, errors.ErrUnsupported }
func (*RegionReader) Close() error                      { return nil }

// FillDecoded answers errors.ErrUnsupported, calling release first.
func (*SpanRecords) FillDecoded(_ context.Context, _ Extent, _ string, _ Decoder, release func()) (*Section, error) {
	return Fill(context.Background(), Extent{}, "", release)
}

func (*Section) Length() int64          { return 0 }
func (*Section) Digest() string         { return "" }
func (*Section) ChunkDigests() []string { return nil }
func (*Section) Extent() Extent         { return Extent{} }
func (*Section) RecordErr() error       { return nil }
func (*Section) Handle() uintptr        { return 0 }
func (*Section) Freed() bool            { return true }
func (*Section) Lend() (*Loan, error)   { return nil, errors.ErrUnsupported }
func (*Section) Close() error           { return nil }

// Loan is unbuilt outside Windows.
type Loan struct{}

func (*Loan) Handle() uintptr         { return 0 }
func (*Loan) Started()                {}
func (*Loan) End()                    {}
func (*Loan) EndWhen(<-chan struct{}) {}
