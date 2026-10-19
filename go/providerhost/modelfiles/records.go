package modelfiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// SpanRecord is OA's own record of an object (INF-MS7): its "sha256:"
// digest, its length and the hex SHA-256 of each ChunkSize span of its
// bytes, made when OA hashed every byte of the object.
type SpanRecord struct {
	Digest string   `json:"digest"`
	Length int64    `json:"length"`
	Spans  []string `json:"spans"`
}

// recordFile is a record as stored, bound to the span size it was made with.
type recordFile struct {
	Version int   `json:"version"`
	Span    int64 `json:"span"`
	SpanRecord
}

// chunkCount is the number of ChunkSize spans of an object of n bytes.
func chunkCount(n int64) int { return int((n + ChunkSize - 1) / ChunkSize) }

// maxRecordBytes bounds one record file: a 1 TB object has 65,536 spans.
const maxRecordBytes = 16 << 20

func (r SpanRecord) check() error {
	if _, err := checkDigest(r.Digest); err != nil {
		return err
	}
	if r.Length <= 0 || len(r.Spans) != chunkCount(r.Length) {
		return fmt.Errorf("modelfiles: record of %d bytes holds %d span digests", r.Length, len(r.Spans))
	}
	for i, s := range r.Spans {
		if _, err := checkDigest("sha256:" + s); err != nil {
			return fmt.Errorf("modelfiles: record span %d: %w", i, err)
		}
	}
	return nil
}

// SpanRecords is the folder in the provider host's private state that holds
// OA's span records, one file per object digest (INF-MS7). The folder is
// private to this account (privatedir); the account is the boundary, as for
// the section itself (INF-MS3).
type SpanRecords struct {
	dir string
	mu  sync.Mutex
}

// tempPrefix starts the name of a record Put is still writing.
const tempPrefix = ".record-"

// OpenSpanRecords opens, and creates when needed, the record folder dir,
// an absolute path in the provider host's private state, through the
// provider kit's one private-folder helper: it creates the folder private
// to this account and refuses one that is a file or a link, that another
// principal owns, or that is open beyond this account (and SYSTEM on
// Windows) (privatedir.ErrNotPrivate). It removes the partly written
// records a killed writer left.
func OpenSpanRecords(dir string) (*SpanRecords, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("modelfiles: the span record folder must be an absolute path")
	}
	if err := privatedir.Ensure(dir); err != nil {
		return nil, fmt.Errorf("modelfiles: the span record folder: %w", err)
	}
	stale, err := filepath.Glob(filepath.Join(dir, tempPrefix+"*"))
	if err != nil {
		return nil, err
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return nil, fmt.Errorf("modelfiles: removing a partly written span record: %w", err)
		}
	}
	return &SpanRecords{dir: filepath.Clean(dir)}, nil
}

func (r *SpanRecords) path(digest string) (string, error) {
	hexed, err := checkDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.dir, hexed+".json"), nil
}

// Lookup answers the record of the object digest names. A missing record,
// and one that does not parse, names another digest, was made with another
// span size or holds the wrong number of spans, is answered as none: the
// next fill hashes the whole object again.
func (r *SpanRecords) Lookup(digest string) (SpanRecord, bool) {
	p, err := r.path(digest)
	if err != nil {
		return SpanRecord{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.Open(p)
	if err != nil {
		return SpanRecord{}, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil || len(data) > maxRecordBytes {
		return SpanRecord{}, false
	}
	var rf recordFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&rf) != nil || rf.Version != 1 || rf.Span != ChunkSize || rf.Digest != digest || rf.check() != nil {
		return SpanRecord{}, false
	}
	return rf.SpanRecord, true
}

// Put writes rec, replacing any record of the same digest. Only OA's own
// hashing makes a record: a fill that hashed every byte, and a packer that
// hashed the object it packed.
func (r *SpanRecords) Put(rec SpanRecord) error {
	if err := rec.check(); err != nil {
		return err
	}
	p, err := r.path(rec.Digest)
	if err != nil {
		return err
	}
	data, err := json.Marshal(recordFile{Version: 1, Span: ChunkSize, SpanRecord: rec})
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tmp, err := os.CreateTemp(r.dir, tempPrefix+"*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Sync(), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		//unchecked: removing a partly written record of this call; the write error is what the caller is told
		os.Remove(tmp.Name())
		return fmt.Errorf("modelfiles: writing the span record of %s: %w", rec.Digest, err)
	}
	return nil
}

// Decoder places an object's decoded bytes in view, all of it. spans are
// OA's record of the object when one exists, which it may check as it goes;
// they are nil on a first fill, when the fill hashes the whole object
// (INF-MS8). A decoder may leave bytes for the fill's hash pass to complete
// and return finish: the fill calls finish(i) once for each span i, on
// several goroutines at once, and hands the function it returns span i's
// bytes in order, one block at a time, each just before hashing it. That
// function completes the block in place and may complete later bytes of
// the same span; it writes no byte of another span. A nil finish, or a nil
// function for a span, leaves the bytes as decode wrote them.
type Decoder func(ctx context.Context, view []byte, spans []string) (finish func(span int) func(block []byte), err error)
