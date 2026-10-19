package modelpack

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// errBaseDigest: the base given is another object than the delta names.
var errBaseDigest = fmt.Errorf("%w: the delta's base", modelfiles.ErrDigest)

// Delta units (inference CONTRACT.md INF-MP12; VISION 2026-10-02;
// research/model-locators/DELTA.md, ANS.md). A fine-tune's unit may be
// stored as the exact integer difference of its BF16 values against the
// same values of a base object: each value's order-preserving key minus the
// base value's key, modulo 2^16, zig-zag coded (int-diff-v1). The words are
// coded exp-ans-v1 in its delta layout, under contexts from the base
// values' exponents, and the decoder undoes the difference as it joins the
// planes. The index names the base by digest and length, one level deep,
// and each delta unit its transform and the offset of its base bytes.
const (
	// TransformIntDiffV1 names the int-diff-v1 transform of a delta unit.
	TransformIntDiffV1 = "int-diff-v1"
)

// ErrBase: the object holds delta units and its proven base was not given.
var ErrBase = errors.New("modelpack: the object is a delta against a base, and its proven base was not given")

// BaseRef names the base object a file's delta units are coded against.
type BaseRef struct {
	Digest string `json:"digest"`
	Length int64  `json:"length"`
}

// ProvenBase is a borrowed view of a base object's bytes and the digest they
// hashed to. It does not own or freeze the bytes: callers must keep the
// backing storage alive and unchanged, through every alias, from the start
// of ProveBase until every operation using the result has returned. This
// includes copies of ProvenBase and concurrent operations. A digest is
// computed once; it is not rechecked when the bytes are read. FillZipWith's
// internal section borrow holds its read-only mapping and loan until the
// fill has finished.
type ProvenBase struct {
	digest string
	b      []byte
}

// ProveBase hashes the whole base object once and returns a ProvenBase that
// borrows b without copying it. The caller must keep b's backing storage
// alive and immutable from entry to ProveBase until every operation using
// the returned value has returned, including operations using copied values
// concurrently. Mutating b or any alias violates this precondition and can
// make the retained bytes no longer match the digest.
func ProveBase(b []byte) ProvenBase {
	sum := sha256.Sum256(b)
	return ProvenBase{digest: "sha256:" + hex.EncodeToString(sum[:]), b: b}
}

// Digest is the base's "sha256:" digest.
func (p ProvenBase) Digest() string { return p.digest }

// DeltaBase is the base Pack codes delta units against: its proven bytes
// and its tensors (ReadTensors). A fine-tune's BF16 tensor finds its base
// tensor by name, and the two must have the same dtype and length.
type DeltaBase struct {
	Base    ProvenBase
	Tensors []Tensor
}

// baseFor returns the base bytes index needs, nil for an index with no base
// (INF-MP12). An index with a base and no proven base given is ErrBase; a
// base of another digest or length is modelfiles.ErrDigest-wrapped
// errBaseDigest. Nothing is decoded before this check.
func baseFor(index Index, base ProvenBase) ([]byte, error) {
	if index.Base == nil {
		return nil, nil
	}
	if base.digest == "" {
		return nil, fmt.Errorf("%w: base %s", ErrBase, index.Base.Digest)
	}
	if base.digest != index.Base.Digest || int64(len(base.b)) != index.Base.Length {
		return nil, fmt.Errorf("%w: the base given is %s of %d bytes, the delta names %s of %d",
			errBaseDigest, base.digest, len(base.b), index.Base.Digest, index.Base.Length)
	}
	return base.b, nil
}

// ord maps a BF16 bit pattern to a key that orders as its value does; it
// flips every bit of a negative value and the sign bit of a positive one.
func ord(v uint16) uint16 { return v ^ (0x8000 | uint16(int16(v)>>15)&0x7fff) }

// unord inverts ord.
func unord(k uint16) uint16 { return k ^ (0x8000 | ^uint16(int16(k)>>15)&0x7fff) }

// intDiff writes int-diff-v1 of the BF16 values f against base values b
// into dst: zigzag(ord(f) - ord(b)).
func intDiff(dst, f, b []byte) {
	for i := 0; i+1 < len(f); i += 2 {
		d := int16(ord(binary.LittleEndian.Uint16(f[i:])) - ord(binary.LittleEndian.Uint16(b[i:])))
		binary.LittleEndian.PutUint16(dst[i:], uint16(d<<1)^uint16(d>>15))
	}
}

// joinIntDiff writes value i from the delta word hi[i]<<8|lo[i] against
// base value i: the undo of int-diff-v1, fused into the plane join.
func joinIntDiff(dst, hi, lo, base []byte) {
	for i := range hi {
		z := uint16(hi[i])<<8 | uint16(lo[i])
		d := uint16(int16(z>>1) ^ -int16(z&1))
		binary.LittleEndian.PutUint16(dst[2*i:], unord(ord(binary.LittleEndian.Uint16(base[2*i:]))+d))
	}
}

// encodeDelta codes unit f against base b as int-diff-v1 words in
// exp-ans-v1's delta layout.
func encodeDelta(f, b []byte) ([]byte, error) {
	words := make([]byte, len(f))
	intDiff(words, f, b)
	return EncodeExpANS(nil, words, "BF16", b)
}

// DecodeDelta decodes a delta unit's payload p into dst, the fine-tune's
// values, against b, the unit's values in the base (INF-MP12): the
// exp-ans-v1 planes under the base's exponents, the difference undone as
// the planes join. A payload that is not a delta-layout exp-ans-v1 unit of
// dst's length, or a base of another length, is ErrCorrupt. A unit that
// does not decode against b is ErrCorrupt naming both causes: the
// contexts come from b's exponents, so a base the delta was not made from
// fails here as a damaged payload does.
func DecodeDelta(dst, p, b []byte) error {
	if len(p) < ansHeaderLen || p[1] != layoutDelta {
		return corrupt("a delta unit is an exp-ans-v1 unit in its delta layout")
	}
	if err := decodeExpANS(dst, p, b, true); err != nil {
		return fmt.Errorf("%w (or not the base this delta was made from)", err)
	}
	return nil
}
