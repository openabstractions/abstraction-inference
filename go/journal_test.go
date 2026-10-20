package inference

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestRequestsInvalidRetainedMetadataIsAbsentWithinReadablePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	lines := []journalLine{
		{}, // Old rows without optional metadata stay absent.
		{Preferred: "kept-model", PreferredHost: "kept-host", Selection: "preferred_route_unavailable", Placement: "local"},
		{Preferred: strings.Repeat("x", 257), Selection: "future-selection", Placement: "future-place"},
		{Preferred: "kept-model", Selection: "bad\nselection", Placement: "hosted"},
		{Preferred: "kept-model", PreferredHost: strings.Repeat("host,", 64) + "host", Selection: "preferred_route_unavailable", Placement: "remote"},
		{Preferred: "kept-model", PreferredHost: strings.Repeat("x", 4097), Selection: "preferred_route_unavailable"},
		{Preferred: "last-valid", Placement: "future-place"},
	}
	var data bytes.Buffer
	for i := range lines {
		line := &lines[i]
		line.Sequence, line.UnixMS, line.Route, line.Operation, line.Host, line.Outcome = int64(i), 1, "native", "op", "kept-host", "completed"
		// Establish individual schema invalidity independently of the retained
		// projection, rather than using an aggregate page overflow as evidence.
		raw := wire.RequestEntry{Entry: line.entry(), PreferredModel: line.Preferred, Selection: line.Selection}
		if line.Selection == "preferred_route_unavailable" {
			raw.UnavailableHosts = strings.Split(line.PreferredHost, ",")
		}
		_, invalid := wire.ToJSON(&raw)
		if (invalid != nil) != (i >= 2 && i <= 5) {
			t.Fatalf("single entry %d validity: %v", i, invalid)
		}
		if err := json.NewEncoder(&data).Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(path, len(lines), nil)
	if err != nil {
		t.Fatal(err)
	}
	p := j.Requests(0, 256)
	if p.Outcome != wire.AuditOutcomePage || len(p.Entries) != len(lines) || p.Next != int64(len(lines)) || !p.AtEnd {
		t.Fatalf("mixed retained page: outcome=%v entries=%d next=%d at_end=%v", p.Outcome, len(p.Entries), p.Next, p.AtEnd)
	}
	for i, entry := range p.Entries {
		if entry.Entry.Sequence != int64(i) || entry.Entry.Host != "kept-host" || entry.Entry.Operation != "op" || entry.Entry.Outcome != "completed" {
			t.Fatalf("neighbouring audit decision changed: %+v", entry.Entry)
		}
		want := wire.ServedPlace(lines[i].Placement)
		if want != "" {
			if entry.Placement == nil || *entry.Placement != want {
				t.Fatalf("valid placement %q changed", want)
			}
		} else if entry.Placement != nil {
			t.Fatalf("unrecorded placement at %d was exposed", i)
		}
	}
	if p.Entries[0].PreferredModel != "" || p.Entries[0].Selection != "" || len(p.Entries[0].UnavailableHosts) != 0 {
		t.Fatal("missing metadata fabricated", p.Entries[0])
	}
	if p.Entries[1].PreferredModel != "kept-model" || p.Entries[1].Selection != "preferred_route_unavailable" || len(p.Entries[1].UnavailableHosts) != 1 || p.Entries[1].UnavailableHosts[0] != "kept-host" {
		t.Fatal("valid metadata changed", p.Entries[1])
	}
	if p.Entries[2].PreferredModel != "" || p.Entries[2].Selection != "future-selection" {
		t.Fatal("preferred-model refusal changed other facts", p.Entries[2])
	}
	if p.Entries[3].PreferredModel != "kept-model" || p.Entries[3].Selection != "" {
		t.Fatal("selection refusal changed other facts", p.Entries[3])
	}
	for _, i := range []int{4, 5} {
		if p.Entries[i].PreferredModel != "kept-model" || p.Entries[i].Selection != "preferred_route_unavailable" || len(p.Entries[i].UnavailableHosts) != 0 {
			t.Fatal("unavailable-host refusal changed other facts", p.Entries[i])
		}
	}
	if _, err := wire.ToJSON(&p); err != nil {
		t.Fatalf("mixed page cannot be encoded: %v", err)
	}
	if p := j.Requests(2, 1); p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 1 || p.Entries[0].PreferredModel != "" || p.Next != 3 || p.AtEnd {
		t.Fatal("invalid metadata changed bounded cursor semantics", p)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, data.Bytes()) {
		t.Fatalf("read changed retained bytes: %v", err)
	}
}

func TestRequestsValidOpenPlacementStillObeysWholePageBudget(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "audit.jsonl"), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, place := range []string{strings.Repeat("x", 600*1024), strings.Repeat("y", 600*1024)} {
		value := wire.ServedPlace(place)
		if _, err := wire.ToJSON(&wire.RequestEntry{Entry: wire.AuditEntry{Route: wire.AuditRouteNative}, Placement: &value}); err != nil {
			t.Fatalf("open placement is individually invalid: %v", err)
		}
		if err := j.Append(Record{Outcome: "completed", Placement: value}); err != nil {
			t.Fatal(err)
		}
	}
	if p := j.Requests(0, 2); p.Outcome != wire.AuditOutcomeUnavailable || len(p.Entries) != 0 || p.Next != 0 || p.AtEnd {
		t.Fatal("aggregate overflow was silently trimmed", p.Outcome, len(p.Entries))
	}
	if p := j.Requests(0, 1); p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 1 || p.Entries[0].Placement == nil || len(*p.Entries[0].Placement) != 600*1024 {
		t.Fatal("valid open word was dropped", p.Outcome, len(p.Entries))
	}
}

func TestRequestsJournalMissingAndStorageErrorRemainDistinct(t *testing.T) {
	missing, err := OpenJournal(filepath.Join(t.TempDir(), "absent.jsonl"), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := missing.Requests(0, 1); p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 0 || p.Next != 0 || !p.AtEnd {
		t.Fatal(p)
	}
	if journal, err := OpenJournal(t.TempDir(), 4, nil); err == nil || journal != nil {
		t.Fatalf("real storage read error became an empty journal: journal=%v err=%v", journal, err)
	}
}

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

// F20: the installed journal keeps a fallback's preferred route across a
// reopen. Requests wraps the unchanged published AuditEntry.
func TestJournalKeepsTheSelectionAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	j, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Record{Operation: "op-1", Host: "ollama", Model: "local-model", Outcome: "completed",
		Preferred: "cloud-model", PreferredHost: "cloud", Selection: "preferred_route_unavailable", Placement: wire.ServedPlaceLocal}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.lines) != 1 {
		t.Fatalf("lines %+v", reopened.lines)
	}
	if l := reopened.lines[0]; l.Host != "ollama" || l.Preferred != "cloud-model" || l.PreferredHost != "cloud" || l.Selection != "preferred_route_unavailable" {
		t.Fatalf("reopened line %+v", l)
	}
	p := reopened.Requests(0, 256)
	if p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 1 {
		t.Fatal(p)
	}
	r := p.Entries[0]
	if r.Entry.Model != "local-model" || r.PreferredModel != "cloud-model" || len(r.UnavailableHosts) != 1 || r.UnavailableHosts[0] != "cloud" || r.Selection != "preferred_route_unavailable" || r.Placement == nil || *r.Placement != wire.ServedPlaceLocal {
		t.Fatal(r)
	}
}

func TestRequestsRetainCursorAndAbsentOldPlacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("{\"sequence\":0,\"unix_ms\":1,\"route\":\"native\",\"outcome\":\"completed\",\"host\":\"old-host\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(path, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p := j.Requests(0, 1); len(p.Entries) != 1 || p.Entries[0].Placement != nil {
		t.Fatal(p)
	}
	for _, place := range []wire.ServedPlace{wire.ServedPlaceHosted, wire.ServedPlaceRemote} {
		if err := j.Append(Record{Outcome: "completed", Placement: place}); err != nil {
			t.Fatal(err)
		}
	}
	if p := j.Requests(0, 256); p.Outcome != wire.AuditOutcomeGap || len(p.Entries) != 0 || p.Next != 1 || p.AtEnd {
		t.Fatal(p)
	}
	p := j.Requests(1, 1)
	if len(p.Entries) != 1 || *p.Entries[0].Placement != wire.ServedPlaceHosted || p.Next != 2 || p.AtEnd {
		t.Fatal(p)
	}
	p = j.Requests(2, 256)
	if len(p.Entries) != 1 || *p.Entries[0].Placement != wire.ServedPlaceRemote || p.Next != 3 || !p.AtEnd {
		t.Fatal(p)
	}
	for _, args := range [][2]int64{{-1, 1}, {4, 1}, {1, 0}, {1, 257}} {
		p := j.Requests(args[0], args[1])
		if p.Outcome != wire.AuditOutcomeInvalid || len(p.Entries) != 0 || p.Next != args[0] || p.AtEnd {
			t.Fatal(p)
		}
	}
}

func TestRequestsOversizeRefusesTheWholePage(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "audit.jsonl"), 256, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if err := j.Append(Record{Outcome: "completed", Reason: strings.Repeat("x", 12000)}); err != nil {
			t.Fatal(err)
		}
	}
	if p := j.Requests(0, 256); p.Outcome != wire.AuditOutcomeUnavailable || len(p.Entries) != 0 || p.Next != 0 || p.AtEnd {
		t.Fatal(p)
	}
	if p := j.Requests(0, 1); p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 1 {
		t.Fatal(p)
	}
	if p := j.Page(0, 256); p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 128 {
		t.Fatal("Audit changed", p.Outcome, len(p.Entries))
	}
}

func TestRequestsPlacementIsCapturedFromTheAdmittedHost(t *testing.T) {
	for _, tc := range []struct {
		host router.Host
		want wire.ServedPlace
	}{{router.Host{}, wire.ServedPlaceLocal}, {router.Host{Hosted: true}, wire.ServedPlaceHosted}, {router.Host{Hosted: true, Domain: "paired"}, wire.ServedPlaceRemote}} {
		op := newOperation("op", Subject{}, &tc.host, "model", "", 1, time.Now())
		tc.host.Hosted = false
		tc.host.Domain = "changed"
		if op.placement != tc.want {
			t.Fatal(op.placement, tc.want)
		}
	}
}

func TestRequestsOptionalMetadataBoundsDoNotHideAuditEntry(t *testing.T) {
	for _, r := range []Record{{Preferred: strings.Repeat("x", 257)}, {PreferredHost: strings.Repeat("host,", 64) + "host", Selection: "preferred_route_unavailable"}, {PreferredHost: strings.Repeat("x", 4097), Selection: "preferred_route_unavailable"}, {Selection: "bad\nselection"}} {
		j, err := OpenJournal(filepath.Join(t.TempDir(), "audit.jsonl"), 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Outcome = "completed"
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
		p := j.Requests(0, 1)
		if p.Outcome != wire.AuditOutcomePage || len(p.Entries) != 1 || p.Next != 1 || !p.AtEnd || p.Entries[0].Entry.Outcome != "completed" {
			t.Fatal(p)
		}
	}
	var entry wire.RequestEntry
	if err := wire.FromJSON([]byte(`{"entry":{"sequence":0,"unix_ms":1,"route":"native","rung":"","program":"","operation":"","host":"","model":"","credential":"","outcome":"completed","reason":"","tokens_in":0,"tokens_out":0,"wall_ms":0,"ceiling":""},"future_fact":true}`), &entry); err != nil {
		t.Fatal("open request entry rejected additive observation", err)
	}
}

func TestRequestsInvalidAuditEntryStillRefusesWholePage(t *testing.T) {
	j, err := OpenJournal(filepath.Join(t.TempDir(), "audit.jsonl"), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Record{Route: "not-a-route", Outcome: "completed", Preferred: strings.Repeat("x", 257)}); err != nil {
		t.Fatal(err)
	}
	if p := j.Requests(0, 1); p.Outcome != wire.AuditOutcomeUnavailable || len(p.Entries) != 0 || p.Next != 0 || p.AtEnd {
		t.Fatal("invalid audit content was concealed", p)
	}
}

func TestRetainedInvalidBaseAuditRefusesWithoutSkippingLaterEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	var retained bytes.Buffer
	for _, line := range []journalLine{
		{Sequence: 0, UnixMS: 1, Route: "invalid-required-route", Outcome: "completed", Operation: "bad-base"},
		{Sequence: 1, UnixMS: 2, Route: "native", Outcome: "completed", Operation: "later-valid", Preferred: "recorded-model"},
	} {
		if err := json.NewEncoder(&retained).Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, retained.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(path, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	requestRefusal := journal.Requests(0, 2)
	if requestRefusal.Outcome != wire.AuditOutcomeUnavailable || len(requestRefusal.Entries) != 0 || requestRefusal.Next != 0 || requestRefusal.AtEnd {
		t.Fatal("invalid required audit fact was hidden", requestRefusal)
	}
	if _, err := wire.ToJSON(&requestRefusal); err != nil {
		t.Fatal(err)
	}
	// An explicit later cursor names the intact later decision. Receiving a
	// corrupt row never chooses that cursor on the caller's behalf.
	requests := journal.Requests(1, 1)
	if requests.Outcome != wire.AuditOutcomePage || len(requests.Entries) != 1 || requests.Entries[0].Entry.Sequence != 1 || requests.Entries[0].Entry.Operation != "later-valid" || requests.Entries[0].PreferredModel != "recorded-model" || requests.Next != 2 || !requests.AtEnd {
		t.Fatal("later request fact changed", requests)
	}
	if _, err := wire.ToJSON(&requests); err != nil {
		t.Fatal(err)
	}
	valid := journal.Page(1, 1)
	if valid.Outcome != wire.AuditOutcomePage || len(valid.Entries) != 1 || valid.Entries[0].Sequence != 1 || valid.Entries[0].Operation != "later-valid" || valid.Next != 2 || !valid.AtEnd {
		t.Fatal(valid)
	}
	if _, err := wire.ToJSON(&valid); err != nil {
		t.Fatal(err)
	}
	page := journal.Page(0, 2)
	if page.Outcome != wire.AuditOutcomeUnavailable || len(page.Entries) != 0 || page.Next != 0 || page.AtEnd {
		_, encodeError := wire.ToJSON(&page)
		t.Fatalf("Audit reported an unencodable page instead of unavailable: outcome=%v entries=%d next=%d encode_error=%v", page.Outcome, len(page.Entries), page.Next, encodeError)
	}
	if _, err := wire.ToJSON(&page); err != nil {
		t.Fatal(err)
	}
	if again := journal.Requests(0, 2); again.Outcome != wire.AuditOutcomeUnavailable || again.Next != 0 || len(again.Entries) != 0 {
		t.Fatal("receiving advanced past or changed the invalid base entry", again)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, retained.Bytes()) {
		t.Fatalf("projection rewrote retained facts: %v", err)
	}
}
