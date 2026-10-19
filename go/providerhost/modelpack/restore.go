package modelpack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"runtime"
	"sync"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// restoreWindow bounds the decoded bytes Restore holds at once.
const restoreWindow = 256 << 20

// Restore writes the object a pack holds to w, in order, decoding its units
// read from src at the index's packed offsets on up to workers goroutines
// (GOMAXPROCS when workers is not positive), a window of at most 256 MiB at a
// time (INF-MP9). It checks every modelfiles.ChunkSize span against the
// index's spans as the bytes pass, and the whole object against the index's
// digest at the end: a span or object of other bytes is modelfiles.ErrDigest.
// A malformed index or unit, or a short read, is ErrCorrupt. The index's
// own spans and digest prove only that the pack is the object its index
// names; a caller that knows the object's digest compares it as well. On an
// error w holds partial output.
func Restore(ctx context.Context, w io.Writer, src io.ReaderAt, index Index, workers int) error {
	return RestoreWithBase(ctx, w, src, index, ProvenBase{}, workers)
}

// RestoreWithBase restores as Restore does an object that may hold delta
// units (INF-MP12): base is its proven base, checked against the index's
// before any unit is read (ErrBase when missing, modelfiles.ErrDigest when
// another object); an object with no base ignores it. Keep the bytes borrowed
// by base immutable and alive until this call returns; its workers may read
// them concurrently.
func RestoreWithBase(ctx context.Context, w io.Writer, src io.ReaderAt, index Index, base ProvenBase, workers int) error {
	if err := modelfiles.CheckDigest(index.Digest); err != nil {
		return fmt.Errorf("%w: index digest: %v", ErrCorrupt, err)
	}
	maxPacked, err := checkIndex(index)
	if err != nil {
		return err
	}
	spans, err := checkSpans(index.Length, index.Spans)
	if err != nil {
		return err
	}
	bb, err := baseFor(index, base)
	if err != nil {
		return err
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	read := readerAt(src)
	p := &prover{spans: spans, whole: sha256.New(), span: sha256.New()}
	units := index.Chunks
	for first := 0; first < len(units); {
		// A window of units no larger than restoreWindow, at least one.
		last, bytes := first, int64(0)
		for last < len(units) && (last == first || bytes+units[last].Length <= restoreWindow) {
			bytes += units[last].Length
			last++
		}
		bufs := make([][]byte, last-first)
		errs := make([]error, last-first)
		var wg sync.WaitGroup
		next := make(chan int)
		for range min(workers, last-first) {
			wg.Go(func() {
				var packed []byte
				for i := range next {
					c := &units[i]
					out := make([]byte, c.Length)
					if c.Encoding == EncodingStored {
						_, errs[i-first] = read(c, out)
					} else {
						if packed == nil {
							packed = make([]byte, maxPacked)
						}
						var b []byte
						if b, errs[i-first] = read(c, packed[:c.Packed]); errs[i-first] == nil {
							var err error
							if c.Transform != "" {
								err = DecodeDelta(out, b, bb[c.BaseOffset:c.BaseOffset+c.Length])
							} else {
								err = decoderFor(c.Encoding)(out, b)
							}
							if err != nil {
								errs[i-first] = fmt.Errorf("unit at %d: %w", c.Offset, err)
							}
						}
					}
					bufs[i-first] = out
				}
			})
		}
		for i := first; i < last; i++ {
			if ctx.Err() != nil {
				break
			}
			next <- i
		}
		close(next)
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		for k, b := range bufs {
			if errs[k] != nil {
				return errs[k]
			}
			if err := p.write(b); err != nil {
				return err
			}
			if _, err := w.Write(b); err != nil {
				return err
			}
		}
		first = last
	}
	if err := p.finish(); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(p.whole.Sum(nil)); got != index.Digest {
		return fmt.Errorf("%w: the pack decodes to %s, its index names %s", modelfiles.ErrDigest, got, index.Digest)
	}
	return nil
}

// prover hashes bytes in object order, checking each span as it completes.
type prover struct {
	spans       [][32]byte
	whole, span hash.Hash
	inSpan      int64
	index       int
}

func (p *prover) write(b []byte) error {
	p.whole.Write(b)
	for len(b) > 0 {
		n := min(int64(len(b)), modelfiles.ChunkSize-p.inSpan)
		p.span.Write(b[:n])
		p.inSpan += n
		b = b[n:]
		if p.inSpan == modelfiles.ChunkSize {
			if err := p.close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// close checks the span just completed.
func (p *prover) close() error {
	var got [32]byte
	p.span.Sum(got[:0])
	if p.index >= len(p.spans) || got != p.spans[p.index] {
		return fmt.Errorf("%w: span %d at byte %d", modelfiles.ErrDigest, p.index, int64(p.index)*modelfiles.ChunkSize)
	}
	p.index++
	p.inSpan = 0
	p.span.Reset()
	return nil
}

// finish checks a last, partial span and that every span was seen.
func (p *prover) finish() error {
	if p.inSpan > 0 {
		if err := p.close(); err != nil {
			return err
		}
	}
	if p.index != len(p.spans) {
		return fmt.Errorf("%w: %d of %d spans", ErrCorrupt, p.index, len(p.spans))
	}
	return nil
}
