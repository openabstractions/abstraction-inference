package inference

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
)

// Ceiling is a credential's daily limit per UTC day: tokens (input plus
// output), spend in currency millionths, requests, images, audio seconds and
// characters. Zero means no limit in that unit. Every positive limit is
// enforced at admission; the provider records usage as each call ends.
type Ceiling struct {
	TokensPerDay       int64 `json:"tokens_per_day,omitempty"`
	MicrosPerDay       int64 `json:"micros_per_day,omitempty"`
	RequestsPerDay     int64 `json:"requests_per_day,omitempty"`
	ImagesPerDay       int64 `json:"images_per_day,omitempty"`
	AudioSecondsPerDay int64 `json:"audio_seconds_per_day,omitempty"`
	CharactersPerDay   int64 `json:"characters_per_day,omitempty"`
}

// Negative reports a limit below zero in any unit.
func (c Ceiling) Negative() bool {
	return c.TokensPerDay < 0 || c.MicrosPerDay < 0 || c.RequestsPerDay < 0 || c.ImagesPerDay < 0 || c.AudioSecondsPerDay < 0 || c.CharactersPerDay < 0
}

type dayCount struct {
	CostUnreported bool   `json:"cost_unreported,omitempty"`
	Day            string `json:"day"`
	Tokens         int64  `json:"tokens"`
	Micros         int64  `json:"micros"`
	Requests       int64  `json:"requests,omitempty"`
	AudioSeconds   int64  `json:"audio_seconds,omitempty"`
	Characters     int64  `json:"characters,omitempty"`
	Images         int64  `json:"images,omitempty"`
}

// Ceilings holds credential and native-program limits and observed counts.
// Program choices and counts share its explicit state file across restarts.
type Ceilings struct {
	mu             sync.Mutex
	path           string
	limits         map[string]Ceiling
	programLimits  map[string]Ceiling
	programs       map[string]programBudgetRecord
	budgetRevision uint64
	counts         map[string]dayCount
	now            func() time.Time
}

// OpenCeilings reads budgets and counts at path, an absolute provider-owned file.
// An absent file starts at zero. now may be nil.
func OpenCeilings(path string, limits map[string]Ceiling, now func() time.Time) (*Ceilings, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("inference: absolute ceiling state path required")
	}
	if now == nil {
		now = time.Now
	}
	c := &Ceilings{path: path, limits: map[string]Ceiling{}, counts: map[string]dayCount{}, programLimits: map[string]Ceiling{}, programs: map[string]programBudgetRecord{}, now: now}
	for name, limit := range limits {
		if limit.Negative() {
			return nil, fmt.Errorf("inference: negative ceiling for %s", name)
		}
		c.limits[name] = limit
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := c.readState(raw); err != nil {
			return nil, fmt.Errorf("inference: unreadable ceiling state: %w", err)
		}
	}
	return c, nil
}

func (c *Ceilings) today() string { return c.now().UTC().Format("2006-01-02") }

func (c *Ceilings) count(name string) dayCount {
	n := c.counts[name]
	if n.Day != c.today() {
		n = dayCount{Day: c.today()}
	}
	return n
}

// Exceeded reports the first unit whose count has reached its ceiling.
func (c *Ceilings) Exceeded(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit, n := c.limits[name], c.count(name)
	return exceeded(limit, n)
}

func exceeded(limit Ceiling, n dayCount) (string, bool) {
	switch {
	case limit.TokensPerDay > 0 && n.Tokens >= limit.TokensPerDay:
		return "tokens", true
	case limit.MicrosPerDay > 0 && n.Micros >= limit.MicrosPerDay:
		return "micros", true
	case limit.RequestsPerDay > 0 && n.Requests >= limit.RequestsPerDay:
		return "requests", true
	case limit.ImagesPerDay > 0 && n.Images >= limit.ImagesPerDay:
		return "images", true
	case limit.AudioSecondsPerDay > 0 && n.AudioSeconds >= limit.AudioSecondsPerDay:
		return "audio_seconds", true
	case limit.CharactersPerDay > 0 && n.Characters >= limit.CharactersPerDay:
		return "characters", true
	}
	return "", false
}

// programBudgetKey stays in this owner's existing count map. It names an
// authenticated subject and a credential, never a relayed program claim.
func programBudgetKey(name string, subject Subject) string {
	//unchecked: string tuples always encode
	raw, _ := json.Marshal([]string{name, subject.Account, subject.Program})
	return fmt.Sprintf("\x00program/%x", sha256.Sum256(raw))
}

// SetProgramLimit applies native configuration through the durable budget owner.
// Its legacy zero value removes the configured choice.
func (c *Ceilings) SetProgramLimit(name string, subject Subject, limit Ceiling) error {
	if name == "" || subject.Account == "" || subject.Program == "" || limit.Negative() {
		return errors.New("inference: credential, proven subject and nonnegative program budget required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var budget *Ceiling
	if limit != (Ceiling{}) {
		budget = &limit
	}
	return c.changeProgramBudget(name, subject, budget)
}

func (c *Ceilings) ExceededProgram(name string, subject Subject) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := programBudgetKey(name, subject)
	return exceeded(c.programLimits[key], c.count(key))
}

func addObservedCount(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// AddForSubject counts one origin response once in both the credential
// total and its proven-program subtotal, in one persisted CAS value. Tokens
// are observed input plus total output; reasoning is already inside output.
func (c *Ceilings) AddForSubject(name string, subject Subject, input, output, micros int64) error {
	if name == "" || subject.Account == "" || subject.Program == "" || input < 0 || output < 0 || micros < 0 {
		return errors.New("inference: invalid program accounting observation")
	}
	return c.addModalitiesForSubject(name, subject, addObservedCount(input, output), micros, 0, 0, 0)
}

// ProgramSpend reports the same UTC-day observed counters, not an estimate of
// tokens or money the provider did not report. Limits are configured separately.
func (c *Ceilings) ProgramSpend(name string, subject Subject) (day string, tokens, micros, requests int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.count(programBudgetKey(name, subject))
	return n.Day, n.Tokens, n.Micros, n.Requests
}

// Add counts one reply, which is one request, and persists the counts.
func (c *Ceilings) Add(name string, tokens, micros int64) error {
	return c.AddUsage(name, tokens, micros, 0)
}

// AddUsage counts one request and its profile-specific units.
func (c *Ceilings) AddUsage(name string, tokens, micros, audioSeconds int64) error {
	return c.AddUnits(name, tokens, micros, audioSeconds, 0)
}

// AddUnits counts one request and all currently implemented modality units.
func (c *Ceilings) AddUnits(name string, tokens, micros, audioSeconds, characters int64) error {
	return c.AddModalities(name, tokens, micros, 0, audioSeconds, characters)
}

// AddModalities counts one request and every modality-specific unit.
func (c *Ceilings) AddModalities(name string, tokens, micros, images, audioSeconds, characters int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addModalities(name, tokens, micros, 1, images, audioSeconds, characters)
	return c.persist()
}

// AddDurableAttempt accounts a possibly charged durable upstream submission
// once. AddDurableImages separately records its produced images, because a
// failed or uncertain paid submission still consumes one request.
func (c *Ceilings) AddDurableAttempt(operation, name string) error {
	return c.addDurableOnce("attempt", operation, name, 1, 0)
}

func (c *Ceilings) DurableAttemptCounted(operation string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, counted := c.counts["\x00durable/attempt/"+operation]
	return counted
}

func (c *Ceilings) AddDurableImages(operation, name string, images int64) error {
	return c.addDurableOnce("images", operation, name, 0, images)
}

func (c *Ceilings) addDurableOnce(stage, operation, name string, requests, images int64) error {
	return c.addDurableForSubject(stage, operation, name, nil, requests, images)
}

func (c *Ceilings) addDurableForSubject(stage, operation, name string, subject *Subject, requests, images int64) error {
	if operation == "" {
		return errors.New("inference: durable operation required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	marker := "\x00durable/" + stage + "/" + operation
	if _, counted := c.counts[marker]; counted {
		return nil
	}
	keys := []string{name}
	programKey := ""
	var previousProgram programBudgetRecord
	hadProgram := false
	if subject != nil {
		programKey = programBudgetKey(name, *subject)
		keys = append(keys, programKey)
		previousProgram, hadProgram = c.programs[programKey]
		if !hadProgram {
			c.programs[programKey] = programBudgetRecord{Credential: name, Subject: *subject}
		}
	}
	before := map[string]dayCount{}
	for _, key := range keys {
		if count, ok := c.counts[key]; ok {
			before[key] = count
		}
		c.addModalities(key, 0, 0, requests, images, 0, 0)
	}
	// Durable image/video adapters currently carry no cost observation. Record
	// this only at terminal accounting, preserving accepted/uncertain work.
	if stage == "images" {
		c.markCostUnreported(programKey)
	}
	c.counts[marker] = dayCount{Day: c.today()}
	if err := c.persist(); err != nil {
		delete(c.counts, marker)
		for _, key := range keys {
			if count, ok := before[key]; ok {
				c.counts[key] = count
			} else {
				delete(c.counts, key)
			}
		}
		if subject != nil {
			if hadProgram {
				c.programs[programKey] = previousProgram
			} else {
				delete(c.programs, programKey)
			}
		}
		return err
	}
	return nil
}

// PruneDurable removes idempotence markers for operations the job store says
// are terminal or gone. Active operations retain markers across UTC rollover.
func (c *Ceilings) PruneDurable(active map[string]bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for key := range c.counts {
		if !strings.HasPrefix(key, "\x00durable/") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(key, "\x00durable/"), "/", 2)
		if len(parts) == 2 && !active[parts[1]] {
			delete(c.counts, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.persist()
}

func (c *Ceilings) addModalities(name string, tokens, micros, requests, images, audioSeconds, characters int64) {
	n := c.count(name)
	n.Tokens = addObservedCount(n.Tokens, tokens)
	n.Micros = addObservedCount(n.Micros, micros)
	n.Requests = addObservedCount(n.Requests, requests)
	n.AudioSeconds = addObservedCount(n.AudioSeconds, audioSeconds)
	n.Characters = addObservedCount(n.Characters, characters)
	n.Images = addObservedCount(n.Images, images)
	c.counts[name] = n
}

func (c *Ceilings) persist(admit ...func() error) error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	return cas.Change(c.path, func([]byte) ([]byte, error) {
		for _, check := range admit {
			if check != nil {
				if err := check(); err != nil {
					return nil, err
				}
			}
		}
		raw, err := json.MarshalIndent(ceilingState{Version: 1, Counts: c.counts, Programs: c.programs, BudgetRevision: c.budgetRevision}, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(raw, '\n'), nil
	})
}

// SetLimit replaces a credential's ceiling; a zero Ceiling removes it. Counts
// are kept.
func (c *Ceilings) SetLimit(name string, limit Ceiling) error {
	if limit.Negative() {
		return fmt.Errorf("inference: negative ceiling for %s", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit == (Ceiling{}) {
		delete(c.limits, name)
		return nil
	}
	c.limits[name] = limit
	return nil
}

// ReplaceLimits atomically installs the current declaration budgets. Removed
// declarations lose their limits; accumulated usage remains unchanged.
func (c *Ceilings) ReplaceLimits(limits map[string]Ceiling) error {
	next := make(map[string]Ceiling, len(limits))
	for name, limit := range limits {
		if limit.Negative() {
			return fmt.Errorf("inference: negative ceiling for %s", name)
		}
		if limit != (Ceiling{}) {
			next[name] = limit
		}
	}
	c.mu.Lock()
	c.limits = next
	c.mu.Unlock()
	return nil
}

// Spend is what a credential has counted today (UTC): the day, tokens and
// currency millionths.
func (c *Ceilings) Spend(name string) (day string, tokens, micros int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.count(name)
	return n.Day, n.Tokens, n.Micros
}

// SpendUnits returns every enforced count for operator reporting.
func (c *Ceilings) SpendUnits(name string) (day string, tokens, micros, requests, images, audioSeconds, characters int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.count(name)
	return n.Day, n.Tokens, n.Micros, n.Requests, n.Images, n.AudioSeconds, n.Characters
}

// State is the log word for a credential's ceiling: none, or the counts and
// limits of today.
func (c *Ceilings) State(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit, ok := c.limits[name]
	if !ok {
		return "none"
	}
	n := c.count(name)
	state := fmt.Sprintf("tokens %d/%d micros %d/%d", n.Tokens, limit.TokensPerDay, n.Micros, limit.MicrosPerDay)
	if limit.RequestsPerDay > 0 {
		state += fmt.Sprintf(" requests %d/%d", n.Requests, limit.RequestsPerDay)
	}
	if limit.AudioSecondsPerDay > 0 {
		state += fmt.Sprintf(" audio_seconds %d/%d", n.AudioSeconds, limit.AudioSecondsPerDay)
	}
	if limit.ImagesPerDay > 0 {
		state += fmt.Sprintf(" images %d/%d", n.Images, limit.ImagesPerDay)
	}
	if limit.CharactersPerDay > 0 {
		state += fmt.Sprintf(" characters %d/%d", n.Characters, limit.CharactersPerDay)
	}
	return state
}
