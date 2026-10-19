package modelpack

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// deltaArchive writes the fine-tune of deltaPair as an archive of delta
// units against its base, and the base as a loose file; it returns their
// paths and the pair.
func deltaArchive(t *testing.T) (archivePath, basePath string, base, tuned []byte) {
	t.Helper()
	base, baseTensors, tuned, tensors := deltaPair()
	dir := t.TempDir()
	basePath = filepath.Join(dir, "base.safetensors")
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	archive, err := WriteZip(&b, []ArchiveSource{{Path: "tuned.safetensors", Data: tuned, WeightFormat: "safetensors", Tensors: tensors,
		Base: &DeltaBase{Base: ProveBase(base), Tensors: baseTensors}}}, Rates{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bases := archive.Bases(); len(bases) != 1 || bases[0].Digest != digestOf(base) {
		t.Fatalf("archive bases %v", bases)
	}
	archivePath = filepath.Join(dir, "tuned.oa.zip")
	if err := os.WriteFile(archivePath, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return archivePath, basePath, base, tuned
}

// fillFile fills a section from a loose file: a proven base.
func fillFile(t *testing.T, path string, data []byte) *modelfiles.Section {
	t.Helper()
	s, err := modelfiles.Fill(context.Background(), modelfiles.Extent{Path: path, Packed: int64(len(data)), Length: int64(len(data)), Method: zip.Store}, digestOf(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// INF-MP12: a fill of a delta archive against its proven base section gives
// the fine-tune's bytes, first fill and under the record. The fill's loan on
// the base ends with the fill: a base section no engine holds is then freed,
// as every section is at its last loan's end (INF-MS5).
func TestFillZipWithBase(t *testing.T) {
	path, basePath, base, tuned := deltaArchive(t)
	recs := records(t)
	for _, fill := range []string{"first fill", "fill under the record"} {
		baseSection := fillFile(t, basePath, base)
		var rel releases
		s, err := FillZipWith(context.Background(), recs, path, digestOf(tuned), FillOptions{Workers: 4, Release: rel.release, Base: baseSection})
		if err != nil {
			t.Fatalf("%s: %v", fill, err)
		}
		if !bytes.Equal(sectionBytes(t, s), tuned) {
			t.Fatalf("%s: the section differs from the fine-tune", fill)
		}
		s.Close()
		if rel.n.Load() != 1 {
			t.Fatalf("%s: release called %d times", fill, rel.n.Load())
		}
		if !baseSection.Freed() {
			t.Fatalf("%s: the fill's loan on the base did not end", fill)
		}
		baseSection.Close()
	}
}

// INF-MP12: a delta fill with no base, or with a base section of another
// object, is refused before a section exists or a unit is read; release is
// called once and no record is written.
func TestFillZipRefusesAMissingOrHostileBase(t *testing.T) {
	path, basePath, base, tuned := deltaArchive(t)
	recs := records(t)
	var rel releases
	if _, err := FillZip(context.Background(), recs, path, digestOf(tuned), 4, rel.release); !errors.Is(err, ErrBase) || rel.n.Load() != 1 {
		t.Fatalf("no base: %v, release %d", err, rel.n.Load())
	}
	other := bytes.Clone(base)
	other[len(other)-3] ^= 0x20
	otherPath := basePath + ".other"
	if err := os.WriteFile(otherPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	otherSection := fillFile(t, otherPath, other)
	defer otherSection.Close()
	rel = releases{}
	_, err := FillZipWith(context.Background(), recs, path, digestOf(tuned), FillOptions{Workers: 4, Release: rel.release, Base: otherSection})
	if !errors.Is(err, modelfiles.ErrDigest) || !strings.Contains(err.Error(), "the delta names") || rel.n.Load() != 1 {
		t.Fatalf("a hostile base: %v, release %d", err, rel.n.Load())
	}
	if _, ok := recs.Lookup(digestOf(tuned)); ok {
		t.Fatal("a refused fill left a record")
	}
}

// INF-MP12: an archive whose index claims another base, filled against that
// base's proven section, fails the fill's proof: the fine-tune is proved as
// every fill is. The other base differs in mantissa bits only: a base of
// other exponents gives the coder other contexts, and the decode refuses
// the stream before the proof (ErrCorrupt).
func TestFillZipDeltaAgainstAnotherBaseFailsTheProof(t *testing.T) {
	path, basePath, base, tuned := deltaArchive(t)
	other := bytes.Clone(base)
	for i := 1000; i < len(other); i += 2 * 4099 {
		other[i] ^= 0x01
	}
	otherPath := basePath + ".other"
	if err := os.WriteFile(otherPath, other, 0o600); err != nil {
		t.Fatal(err)
	}
	claimed := filepath.Join(filepath.Dir(path), "claims-other.zip")
	rewrite(t, path, claimed, func(x *Index) { x.Base = &BaseRef{Digest: digestOf(other), Length: int64(len(other))} })
	for _, recorded := range []bool{false, true} {
		otherSection := fillFile(t, otherPath, other)
		defer otherSection.Close()
		recs := records(t)
		if recorded {
			baseSection := fillFile(t, basePath, base)
			s, err := FillZipWith(context.Background(), recs, path, digestOf(tuned), FillOptions{Workers: 4, Base: baseSection})
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			baseSection.Close()
		}
		var rel releases
		_, err := FillZipWith(context.Background(), recs, claimed, digestOf(tuned), FillOptions{Workers: 4, Release: rel.release, Base: otherSection})
		if !errors.Is(err, modelfiles.ErrDigest) || strings.Contains(err.Error(), "the delta names") || rel.n.Load() != 1 {
			t.Fatalf("recorded %v: another base: %v, release %d", recorded, err, rel.n.Load())
		}
	}
}
