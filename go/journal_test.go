package inference

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// The journal numbers entries without gaps, keeps its newest entries across a
// reopen and a compaction, reads gap below the oldest, drops a torn last line,
// and stores names and counts only.
func TestJournalRetainsNumberedEntriesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	j, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := j.Append(Record{Route: RouteWindow, Rung: "tcp-loopback/test user=bound process=bound path=bound", Program: "/bin/app", Host: "ollama", Outcome: "completed", TokensOut: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	page := j.Page(0, 256)
	if page.Outcome != wire.AuditOutcomePage || len(page.Entries) != 3 || page.Next != 3 || !page.AtEnd || page.Entries[2].TokensOut != 2 || page.Entries[0].Route != wire.AuditRouteWindow {
		t.Fatalf("first page %+v", page)
	}
	if p := j.Page(1, 1); len(p.Entries) != 1 || p.Entries[0].Sequence != 1 || p.Next != 2 || p.AtEnd {
		t.Fatalf("bounded page %+v", p)
	}
	for i := 3; i < 10; i++ {
		if err := j.Append(Record{Outcome: "not_permitted", Reason: "rights:not_granted"}); err != nil {
			t.Fatal(err)
		}
	}
	if p := j.Page(0, 8); p.Outcome != wire.AuditOutcomeGap || p.Next != 6 {
		t.Fatalf("below the oldest %+v", p)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"sequence":10,"unix_ms"`)
	f.Close()
	reopened, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := reopened.Page(6, 256)
	if p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 4 || p.Entries[0].Sequence != 6 || p.Next != 10 || p.Entries[0].Route != wire.AuditRouteNative {
		t.Fatalf("after reopen %+v", p)
	}
	if err := reopened.Append(Record{Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if p := reopened.Page(10, 1); len(p.Entries) != 1 || p.Entries[0].Sequence != 10 {
		t.Fatalf("the sequence after a torn line %+v", p)
	}
	if p := reopened.Page(12, 1); p.Outcome != wire.AuditOutcomeInvalid {
		t.Fatalf("a cursor past the end %+v", p)
	}
	again, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := again.Page(10, 1); len(p.Entries) != 1 || p.Entries[0].Sequence != 10 {
		t.Fatalf("the entry appended after a torn line did not survive a reopen %+v", p)
	}
	raw, _ := os.ReadFile(path)
	if lines := strings.Count(string(raw), "\n"); lines > 8 {
		t.Fatalf("the file was never compacted: %d lines", lines)
	}
}
