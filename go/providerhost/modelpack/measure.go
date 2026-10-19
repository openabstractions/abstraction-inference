package modelpack

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

const (
	// measureValues is each synthetic decode sample: 32 MiB of 16-bit values.
	measureValues = 16 << 20
	// measureDiskBytes bounds the bytes read to measure the disk.
	measureDiskBytes = 1 << 30
	// measureDiskBlock is one read of the disk measurement.
	measureDiskBlock = 16 << 20
	// measureDiskReaders is how many goroutines read at once.
	measureDiskReaders = 4
)

// MeasureRates measures this machine's rates for Pack within about budget
// (INF-MP9): each encoding's aggregate decode rate on GOMAXPROCS goroutines,
// on synthetic weights of a trained layer's shape held in memory, and the
// read rate of the volume diskFile is on, reading at most 1 GiB of diskFile
// through the unbuffered reader a stored fill uses where the platform has
// one. The decode rates exclude span hashing, which a stored fill pays as
// well. The best of the runs that fit each quarter of budget counts. The
// disk clock starts once its readers are open, and each reader reads at
// least one 16 MiB block, so a slow open (a virus scan of a fresh file)
// lengthens the measurement instead of emptying it. A diskFile under
// 16 MiB is read whole, once, as one shorter block; the rate of so short a
// read leans on the volume's latency. A rate that is not
// positive and finite is an error, never a rate: with a zero disk rate Pack
// would encode nothing.
func MeasureRates(ctx context.Context, diskFile string, budget time.Duration) (Rates, error) {
	slot := budget / 4
	var r Rates
	var err error
	samples := []struct {
		rate  *float64
		dtype string
		force Rates
		data  func() []byte
	}{
		{&r.BF16, "BF16", Rates{BF16: 1e18, Disk: 1}, func() []byte { return weights(measureValues, 1, bf16) }},
		{&r.F16, "F16", Rates{F16: 1e18, Disk: 1}, func() []byte { return weights(measureValues, 2, f16) }},
		{&r.BF16Fixed3, "BF16", Rates{BF16Fixed3: 1e18, Disk: 1}, func() []byte { return weights(measureValues, 1, bf16) }},
	}
	for _, s := range samples {
		if *s.rate, err = decodeRate(ctx, s.data(), s.dtype, s.force, slot); err != nil {
			return Rates{}, err
		}
	}
	if r.Disk, err = diskRate(ctx, diskFile, slot); err != nil {
		return Rates{}, err
	}
	return r, nil
}

// minMeasure is the shortest time a measurement counts over. The monotonic
// clock steps about every 0.5 ms on Windows, and one short read or decode
// can time as 0.
const minMeasure = 5 * time.Millisecond

// now is the clock every measurement reads; a test replaces it.
var now = time.Now

// stopwatch times one measurement of the disk or of a decode. It counts
// once at least minMeasure has passed, or slot when slot is shorter, and
// never at 0: a measurement repeats its work until the stopwatch counts.
type stopwatch struct {
	start time.Time
	floor time.Duration
}

func startStopwatch(slot time.Duration) stopwatch {
	return stopwatch{start: now(), floor: min(minMeasure, slot)}
}

func (w stopwatch) elapsed() time.Duration { return now().Sub(w.start) }

// counts reports whether d is long enough to measure over.
func (w stopwatch) counts(d time.Duration) bool { return d > 0 && d >= w.floor }

// rate is n bytes over the time since start; a time that does not count
// or a rate that is not positive and finite is errNoRate.
func (w stopwatch) rate(n int64) (float64, error) {
	d := w.elapsed()
	rate := float64(n) / d.Seconds()
	if !w.counts(d) || !(rate > 0) || math.IsInf(rate, 0) {
		return 0, fmt.Errorf("%w: %d bytes in %v", errNoRate, n, d)
	}
	return rate, nil
}

// decodeRate packs src with the one encoding force offers and decodes it
// from memory until slot has passed, at least once. Each sample decodes
// src as many times as its stopwatch needs to count, and the best sample's
// rate counts.
func decodeRate(ctx context.Context, src []byte, dtype string, force Rates, slot time.Duration) (float64, error) {
	var b bytes.Buffer
	index, err := Pack(&b, src, []Tensor{{Offset: 0, Length: int64(len(src)), DType: dtype}}, force)
	if err != nil {
		return 0, err
	}
	packed := b.Bytes()
	mem := func(c *ChunkEntry, _ []byte) ([]byte, error) {
		return packed[c.PackedOffset : c.PackedOffset+c.Packed], nil
	}
	open := func() (readFunc, func(), error) { return mem, func() {}, nil }
	dst := make([]byte, len(src))
	best := 0.0
	for all := startStopwatch(slot); best == 0 || all.elapsed() < slot; {
		sample := startStopwatch(slot)
		var n int64
		for first := true; first || !sample.counts(sample.elapsed()); first = false {
			if err := unpack(ctx, dst, index, index.Spans, 0, false, open, nil, nil, nil); err != nil {
				return 0, err
			}
			n += int64(len(src))
		}
		rate, err := sample.rate(n)
		if err != nil {
			return 0, fmt.Errorf("%s decode: %w", dtype, err)
		}
		best = max(best, rate)
	}
	return best, nil
}

// diskRate reads diskFile from its start in blocks on several goroutines
// until slot has passed or 1 GiB is read (readRate).
func diskRate(ctx context.Context, diskFile string, slot time.Duration) (float64, error) {
	if !filepath.IsAbs(diskFile) {
		return 0, fmt.Errorf("modelpack: measure the disk with an absolute file path, not %q", diskFile)
	}
	st, err := os.Stat(diskFile)
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return 0, fmt.Errorf("modelpack: %s is not a regular file with bytes to measure the disk with", diskFile)
	}
	// A file under one block is read whole, as one shorter block.
	block := min(measureDiskBlock, st.Size())
	size := min(st.Size(), measureDiskBytes) / block * block
	open := func() (io.ReaderAt, func(), error) {
		r, err := modelfiles.OpenRegion(modelfiles.Extent{Path: diskFile, Packed: st.Size(), Length: st.Size(), Method: zip.Store})
		if errors.Is(err, errors.ErrUnsupported) {
			f, err := os.Open(diskFile)
			if err != nil {
				return nil, nil, err
			}
			//unchecked: closing a read-only handle after the measurement
			return f, func() { f.Close() }, nil
		}
		if err != nil {
			return nil, nil, err
		}
		//unchecked: closing a read-only handle after the measurement
		return r, func() { r.Close() }, nil
	}
	rate, _, err := readRate(ctx, size, block, slot, open)
	if err != nil {
		return 0, fmt.Errorf("modelpack: measuring the disk with %s: %w", diskFile, err)
	}
	return rate, nil
}

// errNoRate: a measurement gave no positive, finite rate.
var errNoRate = errors.New("modelpack: the measurement gave no rate")

// readRate opens one reader per measureDiskReaders, as many as size has
// blocks of block bytes, and only then starts the clock: opening a file a
// virus scanner holds can take longer than slot. Each reader then reads at
// least one block, and more until slot has passed or size is read, reading
// size again from its start until the stopwatch counts. It returns the rate
// and the blocks read; a rate that is not positive and finite is
// errNoRate.
func readRate(ctx context.Context, size, block int64, slot time.Duration, open func() (io.ReaderAt, func(), error)) (float64, int64, error) {
	if block <= 0 || size < block {
		return 0, 0, fmt.Errorf("%w: no whole %d-byte block in %d bytes", errNoRate, block, size)
	}
	readers := int(min(measureDiskReaders, size/block))
	ras := make([]io.ReaderAt, 0, readers)
	for range readers {
		r, done, err := open()
		if err != nil {
			return 0, 0, err
		}
		defer done()
		ras = append(ras, r)
	}
	// Each reader's first block is its own; later blocks are shared out.
	next := atomic.Int64{}
	next.Store(int64(readers) * block)
	var blocks atomic.Int64
	errs := make([]error, readers)
	var wg sync.WaitGroup
	watch := startStopwatch(slot)
	for w, r := range ras {
		wg.Go(func() {
			buf := make([]byte, block)
			off := int64(w) * block
			for {
				if _, err := r.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
					errs[w] = err
					return
				}
				blocks.Add(1)
				if ctx.Err() != nil {
					return
				}
				d := watch.elapsed()
				if d >= slot && watch.counts(d) {
					return
				}
				if off = next.Add(block) - block; off >= size {
					// size is read; until the stopwatch counts, read it
					// again from its start.
					if watch.counts(d) {
						return
					}
					off %= size
				}
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return 0, 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	n := blocks.Load()
	rate, err := watch.rate(n * block)
	if err == nil && n < int64(readers) {
		err = fmt.Errorf("%w: %d blocks for %d readers", errNoRate, n, readers)
	}
	if err != nil {
		return 0, n, err
	}
	return rate, n, nil
}
