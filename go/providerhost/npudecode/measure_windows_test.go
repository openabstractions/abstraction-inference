package npudecode

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
	"github.com/openabstractions/abstraction-inference/go/providerhost/modelpack"
)

// TestMeasureFill fills an archive's first file repeatedly, through the NPU
// or on the CPU alone, and prints the rate; it runs only when
// OA_NPU_MEASURE names the archive. research/model-locators/npu-prototype/
// tools/fill_energy.py runs it twice per mode and reads the Energy Meter
// around each run.
//
//	OA_NPU_MEASURE          archive path
//	OA_NPU_MEASURE_RECORDS  span record folder holding the file's record
//	OA_NPU_MEASURE_MODE     cpu or npu (npu also needs OA_NPU_KIT)
//	OA_NPU_MEASURE_FILLS    fills to run (default 4)
//	OA_NPU_MEASURE_WORKERS  decode goroutines (default GOMAXPROCS)
func TestMeasureFill(t *testing.T) {
	path := os.Getenv("OA_NPU_MEASURE")
	if path == "" {
		t.Skip("OA_NPU_MEASURE names no archive")
	}
	mode := os.Getenv("OA_NPU_MEASURE_MODE")
	fills, _ := strconv.Atoi(os.Getenv("OA_NPU_MEASURE_FILLS"))
	if fills <= 0 {
		fills = 4
	}
	workers, _ := strconv.Atoi(os.Getenv("OA_NPU_MEASURE_WORKERS"))
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	recs, err := modelfiles.OpenSpanRecords(os.Getenv("OA_NPU_MEASURE_RECORDS"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := modelpack.ReadZip(path)
	if err != nil {
		t.Fatal(err)
	}
	file := archive.Files[0]
	digest := file.Index.Digest
	if _, ok := recs.Lookup(digest); !ok {
		t.Fatalf("no span record of %s in the record folder", digest)
	}
	ctx := context.Background()
	var dev modelpack.FixedDevice
	var npu *Device
	switch mode {
	case "stored":
		measureStored(t, file, fills)
		return
	case "touch":
		measureTouch(t, file.Index.Length, fills, workers)
		return
	case "read":
		measureRead(t, file, fills, workers)
		return
	case "cpu-decode", "npu-decode":
		measureDecode(t, ctx, file, mode, fills, workers)
		return
	case "cpu":
	case "npu":
		start := time.Now()
		if npu, err = Open(ctx, Config{Dir: npuKit(t)}); err != nil {
			t.Fatal(err)
		}
		defer npu.Close()
		dev = npu
		fmt.Printf("device %s open and self-tested in %v\n", npu.Info(), time.Since(start).Round(time.Millisecond))
	default:
		t.Fatalf("OA_NPU_MEASURE_MODE %q: cpu or npu", mode)
	}
	var times []time.Duration
	var r modelpack.FillReport
	for range fills {
		start := time.Now()
		s, err := modelpack.FillZipWith(ctx, recs, filepath.Clean(path), digest, modelpack.FillOptions{Workers: workers, Device: dev, Report: &r})
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
		fmt.Printf("fill %v: decode phase %v (device runs %v), hash pass and the rest %v\n", times[len(times)-1].Round(time.Millisecond),
			r.Decode.Round(time.Millisecond), r.DeviceTime.Round(time.Millisecond), (times[len(times)-1] - r.Decode).Round(time.Millisecond))
		if s.Digest() != digest {
			t.Fatalf("section digest %s", s.Digest())
		}
		s.Close()
		if dev != nil && (r.DeviceUnits == 0 || r.Retried || r.DeviceErr != nil) {
			t.Fatalf("fill report %+v", r)
		}
	}
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	gb := float64(file.Index.Length) / 1e9
	fmt.Printf("mode %s, workers %d, GOMAXPROCS %d: %d fills of %.3f GB, best %v (%.2f GB/s), median %v (%.2f GB/s)\n",
		mode, workers, runtime.GOMAXPROCS(0), fills, gb, sorted[0].Round(time.Millisecond), gb/sorted[0].Seconds(),
		sorted[len(sorted)/2].Round(time.Millisecond), gb/sorted[len(sorted)/2].Seconds())
	fmt.Printf("%.3f GB decoded\n", gb*float64(fills))
	fmt.Printf("last fill: %d units on the device in %d batches, %d fixed-code units on the CPU\n", r.DeviceUnits, r.Batches, r.CPUUnits)
	if npu != nil {
		runs, busy := npu.Stats()
		fmt.Printf("NPU: %d runs, %v from start to completion in all, %.2f ms per run\n", runs, busy.Round(time.Millisecond), float64(busy)/1e6/float64(runs))
	}
}

// measureStored fills the original file, OA_NPU_MEASURE_FILE, as a stored
// object: the floor of reads, page commits and hashing with no decode.
func measureStored(t *testing.T, file modelpack.ArchiveFile, fills int) {
	path := os.Getenv("OA_NPU_MEASURE_FILE")
	st, err := os.Stat(path)
	if err != nil || st.Size() != file.Index.Length {
		t.Fatalf("OA_NPU_MEASURE_FILE must be the archive file's original: %v", err)
	}
	var times []time.Duration
	for range fills {
		start := time.Now()
		s, err := modelfiles.Fill(context.Background(), modelfiles.Extent{Path: path, Packed: st.Size(), Length: st.Size(), Method: zip.Store}, file.Index.Digest, nil)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
		s.Close()
	}
	report(t, "stored", times, file.Index.Length)
}

// measureDecode decodes the archive file's whole exp-fixed3-v1 units from
// memory, fills times: on the CPU with DecodeExpFixed3 (escapes applied)
// on `workers` goroutines into one 512 KiB buffer each, or on the NPU as
// many runs as the units fill batches, on the first two batches loaded
// once (the kernel alone; no copy in or out).
func measureDecode(t *testing.T, ctx context.Context, file modelpack.ArchiveFile, mode string, fills, workers int) {
	f, err := os.Open(file.Extent.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var units [][]byte
	for _, c := range file.Index.Chunks {
		if c.Encoding != modelpack.EncodingExpFixed3V1 || c.Length != modelpack.ChunkBytes {
			continue
		}
		b := make([]byte, c.Packed)
		if _, err := f.ReadAt(b, file.Extent.Offset+c.PackedOffset); err != nil {
			t.Fatal(err)
		}
		units = append(units, b)
	}
	var out int64
	var times []time.Duration
	if mode == "cpu-decode" {
		for range fills {
			start := time.Now()
			var next atomic.Int64
			var wg sync.WaitGroup
			for range workers {
				wg.Go(func() {
					dst := make([]byte, modelpack.ChunkBytes)
					for i := int(next.Add(1) - 1); i < len(units); i = int(next.Add(1) - 1) {
						if err := modelpack.DecodeExpFixed3(dst, units[i]); err != nil {
							t.Error(err)
							return
						}
					}
				})
			}
			wg.Wait()
			times = append(times, time.Since(start))
		}
		out = int64(len(units)) * modelpack.ChunkBytes
	} else {
		dev, err := Open(ctx, Config{Dir: npuKit(t)})
		if err != nil {
			t.Fatal(err)
		}
		defer dev.Close()
		for set := range Sets {
			for k := range dev.Slots() {
				copy(dev.In(set)[k*modelpack.FixedSlot:], units[(set*dev.Slots()+k)%len(units)])
			}
		}
		batches := (len(units) + dev.Slots() - 1) / dev.Slots()
		for range fills {
			start := time.Now()
			for b := range batches {
				if err := dev.Decode(ctx, b%Sets); err != nil {
					t.Fatal(err)
				}
			}
			times = append(times, time.Since(start))
		}
		out = int64(batches) * int64(dev.Slots()) * modelpack.ChunkBytes
		runs, busy := dev.Stats()
		fmt.Printf("NPU: %d runs, %.2f ms per run from start to completion\n", runs, float64(busy)/1e6/float64(runs))
	}
	fmt.Printf("%d whole fixed-code units in memory\n", len(units))
	report(t, mode, times, out)
}

// measureTouch creates a pagefile-backed section of n bytes, as a fill's
// section is created (SEC_COMMIT), maps it and writes one byte per 4 KiB
// page from `workers` goroutines: the cost of committing the pages a fill
// writes, with no read, decode or hash.
func measureTouch(t *testing.T, n int64, fills, workers int) {
	const secCommit = 0x8000000
	var times []time.Duration
	for range fills {
		start := time.Now()
		h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE|secCommit, uint32(n>>32), uint32(n), nil)
		if err != nil {
			t.Fatal(err)
		}
		addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE, 0, 0, uintptr(n))
		if err != nil {
			t.Fatal(err)
		}
		view := mapped(addr, int(n))
		pages := int(n / 4096)
		var next atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for {
					first := int(next.Add(256)) - 256 // 1 MiB of pages per step
					if first >= pages {
						return
					}
					for p := first; p < min(first+256, pages); p++ {
						view[p*4096] = 1
					}
				}
			})
		}
		wg.Wait()
		times = append(times, time.Since(start))
		windows.UnmapViewOfFile(addr)
		windows.CloseHandle(h)
	}
	report(t, "touch", times, n)
}

// measureRead reads every unit of the archive file once per pass through
// modelfiles' RegionReader, one positioned read per unit as a fill reads
// them, on `workers` goroutines: the reads alone, at that many in flight.
func measureRead(t *testing.T, file modelpack.ArchiveFile, fills, workers int) {
	var packed int64
	for _, c := range file.Index.Chunks {
		packed += c.Packed
	}
	var times []time.Duration
	for range fills {
		start := time.Now()
		var next atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				r, err := modelfiles.OpenRegion(file.Extent)
				if err != nil {
					t.Error(err)
					return
				}
				defer r.Close()
				buf := make([]byte, modelpack.FixedSlot)
				for i := int(next.Add(1) - 1); i < len(file.Index.Chunks); i = int(next.Add(1) - 1) {
					c := file.Index.Chunks[i]
					if _, err := r.ReadAt(buf[:c.Packed], c.PackedOffset); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		wg.Wait()
		times = append(times, time.Since(start))
	}
	fmt.Printf("%d readers\n", workers)
	report(t, "read", times, packed)
}

func report(t *testing.T, mode string, times []time.Duration, n int64) {
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	gb := float64(n) / 1e9
	fmt.Printf("mode %s, GOMAXPROCS %d: %d passes of %.3f GB, best %v (%.2f GB/s), median %v (%.2f GB/s)\n",
		mode, runtime.GOMAXPROCS(0), len(times), gb, sorted[0].Round(time.Millisecond), gb/sorted[0].Seconds(),
		sorted[len(sorted)/2].Round(time.Millisecond), gb/sorted[len(sorted)/2].Seconds())
	fmt.Printf("%.3f GB decoded\n", gb*float64(len(times)))
}
