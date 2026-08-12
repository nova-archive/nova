package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestJournalSurvivesTheProcessThatWroteIt. The whole point is a record that
// outlives a `run --rm` container and a failed migration, so the file has to be
// on disk, readable back, and in order.
func TestJournalSurvivesTheProcessThatWroteIt(t *testing.T) {
	dir := t.TempDir()
	run := uuid.New()

	j, err := OpenJournal(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{StateStarted, StatePassed} {
		if _, err := j.Record(PhaseApply, s, map[string]any{"schema": 19}); err != nil {
			t.Fatal(err)
		}
	}

	// A DIFFERENT handle, as if the process had exited and come back.
	reopened, err := OpenJournal(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reopened.Events()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("read %d event(s), want 2", len(events))
	}
	if events[0].Sequence != 0 || events[1].Sequence != 1 {
		t.Errorf("sequences = %d, %d", events[0].Sequence, events[1].Sequence)
	}

	// Resuming must not collide under (run_id, sequence).
	e, err := reopened.Record(PhaseVerify, StatePassed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e.Sequence != 2 {
		t.Errorf("resumed at sequence %d, want 2 — a restart that reuses sequence numbers "+
			"cannot be replayed into upgrade_events", e.Sequence)
	}
}

// TestJournalIsNotWorldReadable. It records what an upgrade did to a database,
// including one that then failed.
func TestJournalIsNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	j, err := OpenJournal(dir, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Record(PhasePreflight, StateStarted, nil); err != nil {
		t.Fatal(err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("journal dir mode = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(j.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal file mode = %o, want 600", fi.Mode().Perm())
	}
}

// TestTornTailDoesNotDiscardTheRecord. A crash mid-write is the case this file
// exists for; refusing to read ninety good events because the ninety-first is
// half-written would throw the record away at the moment it matters.
func TestTornTailDoesNotDiscardTheRecord(t *testing.T) {
	dir := t.TempDir()
	run := uuid.New()
	j, err := OpenJournal(dir, run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Record(PhaseApply, StateStarted, nil); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(j.Path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"run_id":"` + run.String() + `","seq`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := j.Events()
	if err != nil {
		t.Fatalf("a torn tail must not make the journal unreadable: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("read %d event(s), want the one complete record", len(events))
	}
}

// TestJournalRejectsAPhaseTheTableWouldReject. An entry that cannot be replayed
// into upgrade_events is not a record — it is a line that will fail the
// backfill later, when there is nothing to be done about it.
func TestJournalRejectsAPhaseTheTableWouldReject(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Record("migrating", StateStarted, nil); err == nil {
		t.Error("an unknown phase must be refused at write time")
	}
	if _, err := j.Record(PhaseApply, "ok", nil); err == nil {
		t.Error("an unknown state must be refused at write time")
	}
}

// TestSecretsAreRedacted. A journal is a file an operator pastes into a bug
// report.
func TestSecretsAreRedacted(t *testing.T) {
	got := Redact(map[string]any{
		"database_url": "postgres://nova:hunter2@db:5432/nova",
		"actor":        "bug",
		"detail": map[string]any{
			"admin_password": "hunter2",
			// A key that is not secret-shaped, whose VALUE happens to carry a
			// DSN. This is the common case: an error string.
			"note": "could not connect to postgres://nova:hunter2@db:5432/nova",
		},
		"args": []string{"--dsn", "postgresql://nova:hunter2@db/nova"},
	})

	flat := flatten(got)
	if strings.Contains(flat, "hunter2") {
		t.Errorf("a secret survived redaction:\n%s", flat)
	}
	if !strings.Contains(flat, "bug") {
		t.Error("redaction removed something that was not a secret")
	}
	// The DSN in a non-secret-shaped key is masked rather than dropped: the
	// host and database are the useful part of that line.
	if !strings.Contains(flat, "db:5432/nova") {
		t.Errorf("masking removed the part of the DSN worth keeping:\n%s", flat)
	}
}

func flatten(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, v := range t {
				b.WriteString(k)
				b.WriteString("=")
				walk(v)
				b.WriteString(" ")
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case []string:
			for _, e := range t {
				b.WriteString(e)
				b.WriteString(" ")
			}
		default:
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(
				strings.ReplaceAll(sprint(t), "\n", " "), "\t", " ")))
		}
	}
	walk(v)
	return b.String()
}

func sprint(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// TestListRunsFindsAnInterruptedRun. A restarted upgrade has to be able to find
// the run it was in the middle of, or it starts a second one and the two halves
// never join.
func TestListRunsFindsAnInterruptedRun(t *testing.T) {
	dir := t.TempDir()
	want := uuid.New()
	j, err := OpenJournal(dir, want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Record(PhaseApply, StateStarted, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	runs, err := ListRuns(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0] != want {
		t.Fatalf("runs = %v, want exactly %v", runs, want)
	}

	if runs, err := ListRuns(filepath.Join(dir, "absent")); err != nil || runs != nil {
		t.Fatalf("a missing directory is not an error: %v %v", runs, err)
	}
}
