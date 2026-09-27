package inference

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// DefaultJournalCapacity is the number of audit entries a journal retains.
const DefaultJournalCapacity = 4096

// journalLine is one audit entry as the journal file stores it.
type journalLine struct {
	Sequence     int64  `json:"sequence"`
	UnixMS       int64  `json:"unix_ms"`
	Route        string `json:"route"`
	Rung         string `json:"rung,omitempty"`
	Program      string `json:"program,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Host         string `json:"host,omitempty"`
	Model        string `json:"model,omitempty"`
	Credential   string `json:"credential,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	TokensIn     int64  `json:"tokens_in,omitempty"`
	TokensOut    int64  `json:"tokens_out,omitempty"`
	AudioSeconds int64  `json:"audio_seconds,omitempty"`
	Characters   int64  `json:"characters,omitempty"`
	Images       int64  `json:"images,omitempty"`
	WallMS       int64  `json:"wall_ms,omitempty"`
	Ceiling      string `json:"ceiling,omitempty"`
	Domain       string `json:"domain,omitempty"`
	Claim        string `json:"claim,omitempty"`
	Profile      string `json:"profile,omitempty"`
}

func (l journalLine) entry() wire.AuditEntry {
	route, _ := wire.ParseAuditRoute(l.Route)
	return wire.AuditEntry{Sequence: l.Sequence, UnixMs: l.UnixMS, Route: route, Rung: l.Rung, Program: l.Program,
		Operation: l.Operation, Host: l.Host, Model: l.Model, Credential: l.Credential, Outcome: l.Outcome, Reason: l.Reason,
		TokensIn: l.TokensIn, TokensOut: l.TokensOut, AudioSeconds: l.AudioSeconds, Characters: l.Characters, Images: l.Images, WallMs: l.WallMS, Ceiling: l.Ceiling, Domain: l.Domain, Claim: l.Claim, Profile: l.Profile}
}

// Journal is the inference audit: one entry per decision from either route,
// numbered without gaps, the newest Capacity kept in an append-only file that
// is compacted when it doubles. It carries what Record carries: names and
// counts, never header values, keys or message content.
type Journal struct {
	mu       sync.Mutex
	path     string
	capacity int
	lines    []journalLine
	written  int
	next     int64
	now      func() time.Time
}

// OpenJournal reads the journal at path, an absolute file the runtime owns. An
// absent file starts at sequence 0; a torn last line, from a crash mid-append,
// is dropped. capacity 0 selects DefaultJournalCapacity; now may be nil.
func OpenJournal(path string, capacity int, now func() time.Time) (*Journal, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("inference: absolute journal path required")
	}
	if capacity <= 0 {
		capacity = DefaultJournalCapacity
	}
	if now == nil {
		now = time.Now
	}
	j := &Journal{path: path, capacity: capacity, now: now}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return j, nil
	case err != nil:
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var line journalLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Sequence < j.next {
			continue
		}
		j.lines = append(j.lines, line)
		j.next = line.Sequence + 1
		j.written++
	}
	if len(j.lines) > capacity {
		j.lines = append([]journalLine(nil), j.lines[len(j.lines)-capacity:]...)
	}
	// A torn last line would join the next append; rewrite the retained entries.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		if err := j.compact(); err != nil {
			return nil, err
		}
	}
	return j, nil
}

// Append records r as the next entry.
func (j *Journal) Append(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	routeWord := r.Route
	if routeWord == "" {
		routeWord = RouteNative
	}
	line := journalLine{Sequence: j.next, UnixMS: j.now().UnixMilli(), Route: routeWord, Rung: r.Rung, Program: r.Program, Operation: r.Operation,
		Host: r.Host, Model: r.Model, Credential: r.Credential, Outcome: r.Outcome, Reason: r.Reason, TokensIn: r.TokensIn,
		TokensOut: r.TokensOut, AudioSeconds: r.AudioSeconds, Characters: r.Characters, Images: r.Images, WallMS: r.WallMS, Ceiling: r.Ceiling, Domain: r.Domain, Claim: r.Claim, Profile: r.Profile}
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	j.next++
	j.lines = append(j.lines, line)
	if len(j.lines) > j.capacity {
		j.lines = append([]journalLine(nil), j.lines[len(j.lines)-j.capacity:]...)
	}
	if j.written+1 > 2*j.capacity {
		return j.compact()
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		j.written++
	}
	return err
}

// compact rewrites the file with the retained entries.
func (j *Journal) compact() error {
	var b bytes.Buffer
	for _, line := range j.lines {
		raw, err := json.Marshal(line)
		if err != nil {
			return err
		}
		//unchecked: bytes.Buffer.Write never returns a non-nil error
		b.Write(raw)
		//unchecked: bytes.Buffer.WriteByte never returns a non-nil error
		b.WriteByte('\n')
	}
	if err := cas.Change(j.path, func([]byte) ([]byte, error) { return b.Bytes(), nil }); err != nil {
		return fmt.Errorf("inference: compact journal: %w", err)
	}
	j.written = len(j.lines)
	return nil
}

// Page reads at most maxEntries entries from sequence cursor.
func (j *Journal) Page(cursor, maxEntries int64) wire.AuditPage {
	if cursor < 0 || maxEntries < 1 || maxEntries > 256 {
		return wire.AuditPage{Outcome: wire.AuditOutcomeInvalid, Entries: []wire.AuditEntry{}, Next: cursor}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	oldest := j.next - int64(len(j.lines))
	if cursor < oldest {
		return wire.AuditPage{Outcome: wire.AuditOutcomeGap, Entries: []wire.AuditEntry{}, Next: oldest}
	}
	if cursor > j.next {
		return wire.AuditPage{Outcome: wire.AuditOutcomeInvalid, Entries: []wire.AuditEntry{}, Next: cursor}
	}
	page := wire.AuditPage{Outcome: wire.AuditOutcomePage, Entries: []wire.AuditEntry{}, Next: cursor}
	for i := cursor - oldest; i < int64(len(j.lines)) && int64(len(page.Entries)) < maxEntries; i++ {
		page.Entries = append(page.Entries, j.lines[i].entry())
		page.Next = j.lines[i].Sequence + 1
	}
	page.AtEnd = page.Next == j.next
	return page
}
