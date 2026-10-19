// Package modelfiles places a model's bytes in memory once and lends them,
// read-only, to the engines a provider launches (VISION 2026-10-01; inference
// CONTRACT.md INF-MS1..INF-MS6; research/model-locators/MEMORY-LENDING.md).
//
// Fill creates an unnamed, pagefile-backed section the provider host owns and
// fills it from an Extent: a whole file, a STORED region of a container, or
// one DEFLATE stream. It writes no file. It hashes every byte it places and
// refuses bytes whose SHA-256 differs from the object's digest. It then
// drops its writable view and replaces its own handle with a map-read one.
// The section's security descriptor grants map-read and query only, and no
// handle in any process can raise itself to write. The proof made at the
// fill holds for the section's lifetime. Every loan is read-only (VISION
// 2026-10-01); no writable loan exists.
//
// Lend gives one engine a read-only, inheritable duplicate of the section for
// its handle list (abstraction-facade supervision Options.Inherit, FAC-P1).
// Each Loan is a lease that ends when the engine's process tree has exited;
// the caller ends it at once with End when the engine does not start.
// With no warm-residency rule for sections, the section is freed when its last
// loan ends, or when its owner closes it with none open, and the caller's
// release function then ends the memory holding it took.
//
// The package is built on Windows. On Linux the same design is a sealed
// memfd an engine maps or opens as /proc/self/fd/N (MEMORY-LENDING.md §8);
// it is not built, and Fill answers errors.ErrUnsupported outside Windows.
package modelfiles

import (
	"archive/zip"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ChunkSize is the span each chunk digest covers, and the unit of one
// positioned read of a stored extent.
const ChunkSize = 16 << 20

// Extent is where an object's bytes are read from: Packed stored bytes at
// Offset of the file at Path, which are Length bytes once decoded. Method is
// archive/zip's zip.Store or zip.Deflate. A whole file is a zip.Store extent
// at offset zero; a STORED zip member is its region; a DEFLATE member is its
// raw stream (RFC 1951) with the member's CRC-32.
type Extent struct {
	Path   string
	Offset int64
	Packed int64
	Length int64
	Method uint16
	// CRC32 is a DEFLATE member's CRC-32 of its decoded bytes, checked as
	// the stream ends.
	CRC32 uint32
}

var (
	// ErrSource: the extent cannot give Length bytes as described: its
	// range lies beyond the file, the stream ends early, runs long, or fails
	// its CRC-32.
	ErrSource = errors.New("modelfiles: source refused")
	// ErrDigest: the filled bytes hash to another digest than the object's.
	// The section is freed before Fill returns.
	ErrDigest = errors.New("modelfiles: digest mismatch")
	// ErrReleased: the section was freed, or its owner closed it.
	ErrReleased = errors.New("modelfiles: section released")
	// ErrRecord: a proven first fill could not write OA's span record. A
	// fill returns no error for it; Section.RecordErr wraps it. The section
	// stays proven, and the next fill hashes the whole object again
	// (INF-MS8).
	ErrRecord = errors.New("modelfiles: span record not written")
)

// CheckDigest accepts only the form storage reports objects by: "sha256:"
// and 64 lower-case hex digits.
func CheckDigest(digest string) error {
	_, err := checkDigest(digest)
	return err
}

func (s Extent) check() error {
	switch {
	case !filepath.IsAbs(s.Path) || strings.IndexByte(s.Path, 0) >= 0:
		return fmt.Errorf("%w: path must be absolute", ErrSource)
	case s.Offset < 0 || s.Packed < 0 || s.Length <= 0:
		return fmt.Errorf("%w: offset %d, packed %d, length %d", ErrSource, s.Offset, s.Packed, s.Length)
	case s.Method == zip.Store && s.Packed != s.Length:
		return fmt.Errorf("%w: stored bytes of two sizes, %d and %d", ErrSource, s.Packed, s.Length)
	case s.Method != zip.Store && s.Method != zip.Deflate:
		return fmt.Errorf("%w: method %d", ErrSource, s.Method)
	}
	return nil
}

// checkDigest accepts only the "sha256:" form storage reports objects by.
func checkDigest(digest string) (string, error) {
	hexed, ok := strings.CutPrefix(digest, "sha256:")
	if raw, err := hex.DecodeString(hexed); !ok || err != nil || len(raw) != 32 || strings.ToLower(hexed) != hexed {
		return "", fmt.Errorf("modelfiles: digest %q is not sha256:<64 lower-case hex>", digest)
	}
	return hexed, nil
}
