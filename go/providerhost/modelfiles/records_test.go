package modelfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

func testRecord(n int64) SpanRecord {
	var spans []string
	for i := range chunkCount(n) {
		s := sha256.Sum256([]byte{byte(i)})
		spans = append(spans, hex.EncodeToString(s[:]))
	}
	d := sha256.Sum256([]byte("object"))
	return SpanRecord{Digest: "sha256:" + hex.EncodeToString(d[:]), Length: n, Spans: spans}
}

// INF-MS7: a record written is the record read back, keyed by digest.
func TestSpanRecordRoundTrip(t *testing.T) {
	r, err := OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord(ChunkSize*2 + 5)
	if _, ok := r.Lookup(rec.Digest); ok {
		t.Fatal("a record before any was written")
	}
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Lookup(rec.Digest)
	if !ok || got.Digest != rec.Digest || got.Length != rec.Length || !slices.Equal(got.Spans, rec.Spans) {
		t.Fatalf("read back %+v, %v", got, ok)
	}
	if _, err := OpenSpanRecords("relative"); err == nil {
		t.Fatal("a relative record folder was accepted")
	}
}

// INF-MS7: the record folder is made private, and a folder that already
// exists open beyond this account, or that is a file, is refused.
func TestARecordFolderOpenBeyondTheAccountIsRefused(t *testing.T) {
	base := t.TempDir()
	own := filepath.Join(base, "records")
	if _, err := OpenSpanRecords(own); err != nil {
		t.Fatalf("a folder the kit makes: %v", err)
	}
	if err := privatedir.CheckDir(own); err != nil {
		t.Fatalf("the kit's own folder: %v", err)
	}
	// On Windows the folder inherits the temporary directory's entries,
	// Administrators among them; elsewhere it is made 0755.
	wide := filepath.Join(base, "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpanRecords(wide); !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a folder open beyond the account: %v", err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSpanRecords(file); !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a folder that is a file: %v", err)
	}
}

// INF-MS7: opening the folder removes records a killed writer left half
// written, and keeps the records.
func TestOpenRemovesPartlyWrittenRecords(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	r, err := OpenSpanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := testRecord(10)
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, tempPrefix+"12345")
	if err := os.WriteFile(stale, []byte(`{"version":1,`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = OpenSpanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the partly written record is still there: %v", err)
	}
	if _, ok := r.Lookup(rec.Digest); !ok {
		t.Fatal("opening removed a record")
	}
}

// INF-MS7: Put refuses a record that does not describe its object, and a
// file that is not a well-formed record of its key is answered as none.
func TestSpanRecordRefusals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	r, err := OpenSpanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	good := testRecord(ChunkSize + 1)
	for name, bad := range map[string]SpanRecord{
		"one span short": {Digest: good.Digest, Length: good.Length, Spans: good.Spans[:1]},
		"upper-case":     {Digest: good.Digest, Length: good.Length, Spans: []string{strings.ToUpper(good.Spans[0]), good.Spans[1]}},
		"no length":      {Digest: good.Digest, Spans: good.Spans},
		"bad digest":     {Digest: "sha256:abc", Length: good.Length, Spans: good.Spans},
	} {
		if err := r.Put(bad); err == nil {
			t.Errorf("%s: Put accepted %+v", name, bad)
		}
	}
	if err := r.Put(good); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, strings.TrimPrefix(good.Digest, "sha256:")+".json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	other := testRecord(5)
	other.Digest = "sha256:" + strings.Repeat("0", 64)
	for name, body := range map[string]string{
		"another span size": strings.Replace(string(data), `"span":16777216`, `"span":1048576`, 1),
		"another digest":    strings.Replace(string(data), good.Digest, other.Digest, 1),
		"unknown field":     strings.Replace(string(data), `{"version"`, `{"extra":1,"version"`, 1),
		"truncated":         string(data[:len(data)-2]),
	} {
		if body == string(data) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.Lookup(good.Digest); ok {
			t.Errorf("%s: a malformed record was trusted", name)
		}
	}
}
