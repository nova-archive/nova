package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The bootstrap journal (P2-M7.3, D-M7.3-9b).
//
// # Why a file at all, when there is a table for this
//
// Migration 0019 CREATES upgrade_runs. The first upgrade from schema 18 cannot
// insert its `started` row before applying the migration that makes the row
// possible, and an upgrade that only starts recording after it succeeds has no
// record of the case worth recording.
//
// So the sequence is: write and fsync a local journal carrying the run id and
// the preflight/apply-started events → apply → backfill the run and its
// pre-0019 events under the SAME run id → continue recording to both. If the
// apply fails, the journal is the authoritative record, and it is the only one
// that exists.
//
// # Where it lives is designed, not assumed
//
// nova-admin and the migrate service are `run --rm`. A journal on their root
// filesystem evaporates the moment the container exits — which is precisely the
// failed upgrade the operator needs it for. The directory is an operator-owned
// bind mount or named volume, created 0700, files 0600, and every write is
// atomic-append plus fsync so a crash cannot leave a half-line that parses as
// an event.

// JournalDirEnv names the environment variable that points at the journal
// directory. It is deliberately not derived from anything: an operator who
// mounts a volume needs to be able to say where.
const JournalDirEnv = "NOVA_UPGRADE_JOURNAL_DIR"

// DefaultJournalDir is where the Compose deployment mounts the volume.
const DefaultJournalDir = "/var/lib/nova/upgrade"

// Phases and states, matching upgrade_events' CHECK constraints exactly. A
// journal entry that cannot be replayed into the table is not a record.
const (
	PhasePreflight = "preflight"
	PhaseApply     = "apply"
	PhaseVerify    = "verify"
	PhaseRollback  = "rollback"

	StateStarted     = "started"
	StatePassed      = "passed"
	StateFailed      = "failed"
	StateAborted     = "aborted"
	StateSkipped     = "skipped"
	StateInterrupted = "interrupted"
)

// Event is one journalled phase transition. The field names match
// upgrade_events' columns so the backfill is a transcription rather than a
// translation.
type Event struct {
	RunID    string         `json:"run_id"`
	Sequence int64          `json:"sequence"`
	Phase    string         `json:"phase"`
	State    string         `json:"state"`
	Detail   map[string]any `json:"detail,omitempty"`
	At       time.Time      `json:"at"`
}

// Journal is an append-only record for one run.
type Journal struct {
	mu   sync.Mutex
	path string
	run  uuid.UUID
	seq  int64
}

// OpenJournal creates or reopens the journal for a run.
//
// Reopening MATTERS: an interrupted upgrade is restarted with the same run id
// so its events join up, and the sequence resumes past what is already on disk
// rather than restarting at zero and colliding under (run_id, sequence).
func OpenJournal(dir string, run uuid.UUID) (*Journal, error) {
	if dir == "" {
		return nil, errors.New("upgrade: no journal directory. This is the record that survives " +
			"a failed migration, and the containers that write it are `run --rm`, so it has to " +
			"be somewhere the operator owns")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, run.String()+".jsonl"), run: run}

	existing, err := j.Events()
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.Sequence >= j.seq {
			j.seq = e.Sequence + 1
		}
	}
	return j, nil
}

// Path is where the journal lives, so a failure message can tell the operator
// where to look.
func (j *Journal) Path() string { return j.path }

// RunID is the join key shared by the journal, the upgrade_runs row and every
// plane report.
func (j *Journal) RunID() uuid.UUID { return j.run }

// Record appends one event, redacting the detail map, and fsyncs before
// returning. An event that is only in the page cache is not a record.
func (j *Journal) Record(phase, state string, detail map[string]any) (Event, error) {
	if err := validPhaseState(phase, state); err != nil {
		return Event{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	e := Event{
		RunID: j.run.String(), Sequence: j.seq, Phase: phase, State: state,
		Detail: Redact(detail), At: time.Now().UTC(),
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}

	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return Event{}, err
	}
	j.seq++
	return e, nil
}

// Events reads the journal back in sequence order.
//
// A trailing partial line is DROPPED rather than being an error. A crash
// mid-write is the case this file exists for, and refusing to read the ninety
// good events because the tenth is half-written would throw away the record at
// the exact moment it matters.
func (j *Journal) Events() ([]Event, error) {
	b, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Event
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // a torn tail, not a corrupt journal
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Sequence < out[b].Sequence })
	return out, nil
}

// ListRuns reports the run ids with journals in dir, newest file first. It is
// how a restarted upgrade finds the run it was in the middle of.
func ListRuns(dir string) ([]uuid.UUID, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type row struct {
		id uuid.UUID
		at time.Time
	}
	var rows []row
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		if name == e.Name() {
			continue
		}
		id, err := uuid.Parse(name)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rows = append(rows, row{id, info.ModTime()})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].at.After(rows[b].at) })
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.id)
	}
	return out, nil
}

func validPhaseState(phase, state string) error {
	switch phase {
	case PhasePreflight, PhaseApply, PhaseVerify, PhaseRollback:
	default:
		return fmt.Errorf("upgrade: %q is not a phase upgrade_events accepts", phase)
	}
	switch state {
	case StateStarted, StatePassed, StateFailed, StateAborted, StateSkipped, StateInterrupted:
		return nil
	default:
		return fmt.Errorf("upgrade: %q is not a state upgrade_events accepts", state)
	}
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

// secretish matches key names whose values must never be written down. It is a
// denylist, which is the weaker shape, so the detail maps this package builds
// are constructed from known-safe fields and this is the second line rather
// than the first.
var secretish = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|private[_-]?key|credential|dsn|database_url)`)

// dsnPassword matches the password in a postgres URL. A DSN reaches this code
// legitimately — it is how the migration runs — and a journal is a file an
// operator will paste into a bug report.
var dsnPassword = regexp.MustCompile(`(?i)((?:postgres|postgresql)://[^:/@\s]+):[^@\s]*@`)

// Redact returns a copy of detail with secret-shaped keys removed and DSN
// passwords masked. It recurses, because a nested map is exactly where a
// forgotten secret ends up.
func Redact(detail map[string]any) map[string]any {
	if detail == nil {
		return nil
	}
	out := make(map[string]any, len(detail))
	for k, v := range detail {
		if secretish.MatchString(k) {
			out[k] = "[redacted]"
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case string:
		return dsnPassword.ReplaceAllString(t, "$1:[redacted]@")
	case map[string]any:
		return Redact(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e)
		}
		return out
	case []string:
		out := make([]string, len(t))
		for i, e := range t {
			out[i] = dsnPassword.ReplaceAllString(e, "$1:[redacted]@")
		}
		return out
	default:
		return v
	}
}

// WriteSummary renders a human-readable summary next to the journal, for the
// operator who is reading a directory rather than a JSONL file.
func WriteSummary(w io.Writer, events []Event) {
	for _, e := range events {
		fmt.Fprintf(w, "%s  %-9s %-11s seq=%d", e.At.Format(time.RFC3339), e.Phase, e.State, e.Sequence)
		if len(e.Detail) > 0 {
			keys := make([]string, 0, len(e.Detail))
			for k := range e.Detail {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, " %s=%v", k, e.Detail[k])
			}
		}
		fmt.Fprintln(w)
	}
}
