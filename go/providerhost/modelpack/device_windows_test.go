package modelpack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// fixedArchive writes src as an archive of fixed-code units and returns
// its path.
func fixedArchive(t *testing.T, src []byte, tensors []Tensor) string {
	t.Helper()
	var b bytes.Buffer
	archive, err := WriteZip(&b, []ArchiveSource{{Path: "model.safetensors", Data: src, WeightFormat: "safetensors", Tensors: tensors}}, fixedOnly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if encodings(archive.Files[0].Index)[EncodingExpFixed3V1] == 0 {
		t.Fatal("no unit took the fixed code")
	}
	path := filepath.Join(t.TempDir(), "model.oa.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// INF-MP10: a fill through a device gives the object's bytes and spans, on
// a first fill and under the record, and reports the units the device
// decoded.
func TestFillZipWithDevice(t *testing.T) {
	src, tensors := fusedObject()
	path := fixedArchive(t, src, tensors)
	recs := records(t)
	dev := newFakeDevice(8, 2)
	for _, fill := range []string{"first fill", "fill under the record"} {
		var r FillReport
		var rel releases
		s, err := FillZipWith(context.Background(), recs, path, digestOf(src), FillOptions{Workers: 4, Release: rel.release, Device: dev, Report: &r})
		if err != nil {
			t.Fatalf("%s: %v", fill, err)
		}
		if !bytes.Equal(sectionBytes(t, s), src) {
			t.Fatalf("%s: the section differs from the object", fill)
		}
		s.Close()
		if r.DeviceUnits == 0 || r.CPUUnits != 0 || r.Retried || r.DeviceErr != nil || r.Decode <= 0 || r.DeviceTime <= 0 || rel.n.Load() != 1 || dev.busy.Load() {
			t.Fatalf("%s: report %+v, release %d, busy %v", fill, r, rel.n.Load(), dev.busy.Load())
		}
	}
}

// INF-MP10: device output the proof refuses is never kept. The fill runs
// again on the CPU, is proven, and the device is distrusted; release is
// called once.
func TestFillZipWithDeviceRefusedByTheProof(t *testing.T) {
	src, tensors := fusedObject()
	path := fixedArchive(t, src, tensors)
	for _, recorded := range []bool{false, true} {
		recs := records(t)
		if recorded {
			if s, err := FillZip(context.Background(), recs, path, digestOf(src), 4, nil); err != nil {
				t.Fatal(err)
			} else {
				s.Close()
			}
		}
		dev := newFakeDevice(8, 2)
		dev.flipAt = 1
		var r FillReport
		var rel releases
		s, err := FillZipWith(context.Background(), recs, path, digestOf(src), FillOptions{Workers: 4, Release: rel.release, Device: dev, Report: &r})
		if err != nil {
			t.Fatalf("recorded %v: %v", recorded, err)
		}
		if !bytes.Equal(sectionBytes(t, s), src) {
			t.Fatalf("recorded %v: the section differs from the object", recorded)
		}
		s.Close()
		if !r.Retried || r.DeviceUnits == 0 || r.CPUUnits == 0 || dev.Err() == nil || rel.n.Load() != 1 {
			t.Fatalf("recorded %v: report %+v, device error %v, release %d", recorded, r, dev.Err(), rel.n.Load())
		}
		// A distrusted device is not used again.
		runs := dev.decodes.Load()
		s, err = FillZipWith(context.Background(), recs, path, digestOf(src), FillOptions{Workers: 4, Device: dev, Report: &r})
		if err != nil || dev.decodes.Load() != runs || r.DeviceUnits != 0 {
			t.Fatalf("recorded %v: fill after distrust: %v, %d runs, report %+v", recorded, err, dev.decodes.Load()-runs, r)
		}
		s.Close()
	}
}

// INF-MP10: a pack of other bytes is refused whether or not a device
// decoded it, and a refusal the CPU fill repeats leaves the device trusted.
func TestFillZipWithDeviceForgedPack(t *testing.T) {
	src, tensors := fusedObject()
	genuine := fixedArchive(t, src, tensors)
	recs := records(t)
	s, err := FillZip(context.Background(), recs, genuine, digestOf(src), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	changed := slices.Clone(src)
	changed[2000] ^= 0x40
	other := fixedArchive(t, changed, tensors)
	forgedPath := filepath.Join(t.TempDir(), "forged.zip")
	rewrite(t, other, forgedPath, func(x *Index) { x.Digest = digestOf(src) })
	dev := newFakeDevice(8, 2)
	var r FillReport
	var rel releases
	_, err = FillZipWith(context.Background(), recs, forgedPath, digestOf(src), FillOptions{Workers: 4, Release: rel.release, Device: dev, Report: &r})
	if !errors.Is(err, modelfiles.ErrDigest) || !r.Retried || dev.Err() != nil || rel.n.Load() != 1 {
		t.Fatalf("forged pack: %v, report %+v, device error %v, release %d", err, r, dev.Err(), rel.n.Load())
	}
}

// INF-MP7: escape positions altered between the decode and the hash pass
// fail the fill's proof, on a first fill and under the record, on the CPU
// and through a device, and release is called once.
func TestAlteredEscapePositionsFailTheProof(t *testing.T) {
	src, tensors := fusedObject()
	path := fixedArchive(t, src, tensors)
	recorded := records(t)
	if s, err := FillZip(context.Background(), recorded, path, digestOf(src), 4, nil); err != nil {
		t.Fatal(err)
	} else {
		s.Close()
	}
	t.Cleanup(func() { beforeEscapePass = nil })
	for name, f := range tamperings {
		found := true
		beforeEscapePass = func(patches []*fixedPatch) { found = tamper(patches, f) }
		for _, c := range []struct {
			fill string
			recs *modelfiles.SpanRecords
			dev  FixedDevice
		}{
			{"first fill", records(t), nil},
			{"under the record", recorded, nil},
			{"through a device", recorded, newFakeDevice(8, 2)},
		} {
			var rel releases
			var r FillReport
			s, err := FillZipWith(context.Background(), c.recs, path, digestOf(src), FillOptions{Workers: 4, Release: rel.release, Device: c.dev, Report: &r})
			if !found {
				t.Fatalf("%s: no unit to alter", name)
			}
			if err == nil {
				s.Close()
				t.Fatalf("%s, %s: the fill was proven", name, c.fill)
			}
			if !errors.Is(err, modelfiles.ErrDigest) || rel.n.Load() != 1 {
				t.Fatalf("%s, %s: %v, release %d", name, c.fill, err, rel.n.Load())
			}
			if c.dev != nil && (!r.Retried || c.dev.Err() != nil) {
				t.Fatalf("%s, %s: report %+v, device error %v; the CPU rerun fails too and the device stays trusted", name, c.fill, r, c.dev.Err())
			}
		}
	}
	beforeEscapePass = nil
	if _, ok := recorded.Lookup(digestOf(src)); !ok {
		t.Fatal("the record is gone")
	}
	s, err := FillZip(context.Background(), recorded, path, digestOf(src), 4, nil)
	if err != nil || !bytes.Equal(sectionBytes(t, s), src) {
		t.Fatalf("an unaltered fill after the refusals: %v", err)
	}
	s.Close()
}

// INF-MP10: a fill finding the device held by another fill decodes on the
// CPU and says so.
func TestFillZipWithBusyDevice(t *testing.T) {
	src, tensors := fusedObject()
	path := fixedArchive(t, src, tensors)
	dev := newFakeDevice(8, 2)
	dev.TryAcquire()
	var r FillReport
	s, err := FillZipWith(context.Background(), records(t), path, digestOf(src), FillOptions{Workers: 4, Device: dev, Report: &r})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !r.Busy || r.DeviceUnits != 0 || r.CPUUnits == 0 || dev.decodes.Load() != 0 || !bytes.Equal(sectionBytes(t, s), src) {
		t.Fatalf("busy device: report %+v, %d runs", r, dev.decodes.Load())
	}
}
