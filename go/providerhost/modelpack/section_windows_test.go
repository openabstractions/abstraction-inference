package modelpack

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// INF-MP1 with INF-MS2: the pack index's span digests are the digests a
// memory section records when the same object fills it, so the proof has
// one owner and one granularity.
func TestSpansEqualSectionChunkDigests(t *testing.T) {
	src, tensors := object()
	index, _ := pack(t, src, tensors, fast)
	path := filepath.Join(t.TempDir(), "object.bin")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	n := int64(len(src))
	s, err := modelfiles.Fill(context.Background(),
		modelfiles.Extent{Path: path, Packed: n, Length: n, Method: zip.Store},
		"sha256:"+hex.EncodeToString(sum[:]), nil)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	defer s.Close()
	if got := s.ChunkDigests(); !slices.Equal(got, index.Spans) {
		t.Fatalf("section digests %v, pack spans %v", got, index.Spans)
	}
}
