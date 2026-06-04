# M10 Master-Key Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-wrap every secret (per-blob DEKs + signing keys) from an old operator master-key version to the new active version — online, in parallel, with no read-path downtime — driven by `novactl keys rotate-master` + `/api/v1/admin/keys/rotate-master`.

**Architecture:** A new `internal/masterkey.Rotator` runs as one serialized in-process goroutine (`Run(ctx)`, beside the M8/M9 workers). `Run` first resumes any interrupted rotation, then waits for a trigger. `Start` validates (`to == active label`, both keys loaded), atomically marks the source version `rotating`, and enqueues a drain. The drain runs `N` parallel workers that claim DEK batches (`FOR UPDATE SKIP LOCKED`), re-wrap each row with one atomic version-guarded `UPDATE` (unwrap-old → wrap-active, zeroing the transient plaintext), pace between batches, then re-wrap the signing keys, then retire the source version. A `/readyz` check degrades when a `rotating` version's key is not loaded (stall detection). Reads work against either version throughout because the keystore unwraps each row by its recorded version id.

**Tech Stack:** Go, PostgreSQL (sqlc, pgx/v5), XChaCha20-Poly1305 (existing `internal/envelope`), chi router, testcontainers + nginx integration harness.

**Design:** `docs/superpowers/specs/2026-06-03-phase1-m10-master-key-rotation-design.md` is the source of truth; read its "The re-wrap worker", "Operator workflow", and "Stalled-rotation observability" sections before starting.

---

## File Structure

**Created:**
- `internal/db/queries/masterkey.sql` — claim/rewrap/count DEKs + signing keys; master-version get/list/state transitions.
- `internal/masterkey/rotator.go` — `Rotator`: errors, `Start`, `Run`, drain (DEK pool + signing keys + retire), `Status`, `Readyz`, transient-key zeroing.
- `internal/masterkey/rotator_test.go` — `dbtest`-backed unit tests.
- `internal/api/handlers/masterkey_admin.go` — `MasterKeyAdminHandler`: `RotateMaster` + `RotationStatus`.
- `internal/api/handlers/masterkey_admin_test.go`.
- `internal/integration/m10_master_key_rotation_test.go` — two-boot nginx-fronted e2e.

**Modified:**
- `internal/envelope/keystore.go` — `HasLabel`/`LoadedLabels`/`VersionID`/`ActiveVersionID` accessors.
- `internal/db/gen/*` — regenerated.
- `internal/api/server.go` — mount the two admin routes (operator-only) + `ServerConfig.MasterKeyAdmin`.
- `pkg/coordinator/coordinator.go` — build the Rotator + handler; `master_key_rotation` ReadyCheck; `go rotator.Run(ctx)`; `MasterKeyRotation` config.
- `cmd/coordinator/main.go` — `NOVA_MASTER_KEY_REWRAP_{CONCURRENCY,BATCH,PACE_MS}` env knobs.
- `cmd/novactl/main.go` — `keys` subcommand group + usage + package doc.
- `internal/config/types.go` — `MasterKeyRotation` section.
- Docs: `ENCRYPTION_ENVELOPE.md`, `DATA_MODEL.sql`, `openapi.yaml`, `OPERATOR_CHECKLIST.md`, `THREAT_MODEL.md`, `ROADMAP.md`.

---

## Task 0: sqlc queries + regenerate

**Files:**
- Create: `internal/db/queries/masterkey.sql`
- Modify: `internal/db/gen/*` (generated)

- [ ] **Step 1: Write `internal/db/queries/masterkey.sql`.** All tables already exist in `0001_init.sql`; this is queries only. The two re-wrap UPDATEs use named args (two references to `master_key_version_id`); the rest are positional, matching the existing query style.

```sql
-- name: GetMasterVersionByLabel :one
SELECT id, version_label, state, created_at, retired_at
FROM master_key_versions
WHERE version_label = $1;

-- name: GetRotatingVersion :one
SELECT id, version_label, state, created_at, retired_at
FROM master_key_versions
WHERE state = 'rotating'
ORDER BY created_at
LIMIT 1;

-- name: BeginVersionRotation :execrows
-- Atomically mark the source version 'rotating' iff it is currently 'active'
-- and no other version is already rotating. 0 rows ⇒ caller maps to 409/400.
UPDATE master_key_versions
SET state = 'rotating'
WHERE version_label = $1
  AND state = 'active'
  AND NOT EXISTS (SELECT 1 FROM master_key_versions WHERE state = 'rotating');

-- name: RetireVersion :exec
UPDATE master_key_versions
SET state = 'retired', retired_at = now()
WHERE version_label = $1 AND state = 'rotating';

-- name: ListMasterVersions :many
SELECT
  v.version_label,
  v.state,
  v.retired_at,
  (SELECT count(*) FROM data_encryption_keys d
     WHERE d.master_key_version_id = v.id AND d.state IN ('active','rotating')) AS dek_count,
  (SELECT count(*) FROM signing_keys s
     WHERE s.master_key_version_id = v.id AND s.state IN ('active','retired')) AS signing_count
FROM master_key_versions v
ORDER BY v.created_at;

-- name: ClaimDEKsForRewrap :many
-- Claim a batch of re-wrappable DEK ids for a version. FOR UPDATE SKIP LOCKED
-- gives clean N-worker parallelism; run inside the per-batch tx so the locks
-- are held until commit. Served by dek_master_version_idx.
SELECT id, wrapped_key
FROM data_encryption_keys
WHERE master_key_version_id = $1 AND state IN ('active','rotating')
ORDER BY id
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: RewrapDEK :execrows
-- Atomic, version-guarded re-wrap: wrapped_key + master_key_version_id flip
-- together; the old-version guard makes it idempotent and race-safe.
UPDATE data_encryption_keys
SET wrapped_key = sqlc.arg(wrapped_key), master_key_version_id = sqlc.arg(new_version_id)
WHERE id = sqlc.arg(id) AND master_key_version_id = sqlc.arg(old_version_id);

-- name: CountDEKsForVersion :one
SELECT count(*) FROM data_encryption_keys
WHERE master_key_version_id = $1 AND state IN ('active','rotating');

-- name: ListSigningKeysForRewrap :many
SELECT kid, wrapped_key
FROM signing_keys
WHERE master_key_version_id = $1 AND state IN ('active','retired');

-- name: RewrapSigningKey :execrows
UPDATE signing_keys
SET wrapped_key = sqlc.arg(wrapped_key), master_key_version_id = sqlc.arg(new_version_id)
WHERE kid = sqlc.arg(kid) AND master_key_version_id = sqlc.arg(old_version_id);

-- name: CountSigningKeysForVersion :one
SELECT count(*) FROM signing_keys
WHERE master_key_version_id = $1 AND state IN ('active','retired');
```

- [ ] **Step 2: Regenerate.**

Run: `make sqlc-generate`
Expected: `internal/db/gen/masterkey.sql.go` appears with `GetMasterVersionByLabel`, `BeginVersionRotation` (returns `int64` via `:execrows`), `ClaimDEKsForRewrap` (returns `[]ClaimDEKsForRewrapRow{ID uuid.UUID, WrappedKey []byte}`), `RewrapDEK` (`RewrapDEKParams{WrappedKey []byte, NewVersionID uuid.UUID, ID uuid.UUID, OldVersionID uuid.UUID}`), `ListMasterVersions` (`[]ListMasterVersionsRow{VersionLabel string, State gen.KeyState, RetiredAt pgtype.Timestamptz, DekCount int64, SigningCount int64}`), and the signing-key equivalents.

- [ ] **Step 3: codegen-check + commit.**

Run: `make codegen-check && go build ./...`
Expected: clean.

```bash
git add internal/db/queries/masterkey.sql internal/db/gen
git commit -m "feat(db): master-key rotation queries (claim/rewrap/count + version state) (sqlc)"
```

---

## Task 1: keystore accessors

**Files:**
- Modify: `internal/envelope/keystore.go`
- Test: `internal/envelope/keystore_test.go`

- [ ] **Step 1: Write the failing test.** Add to `keystore_test.go`. Build a keystore with two labels via the existing test helper pattern (set `NOVA_MASTER_KEY_V1`/`_V2` + `NOVA_MASTER_KEY_ACTIVE=v2`, call `NewKeystoreFromEnv` + `Bootstrap`; reuse whatever the file's other tests use to construct one).

```go
func TestKeystoreAccessors(t *testing.T) {
	ks, _ := newTwoVersionKeystore(t) // v1 + v2 loaded, active = v2 (helper; mirror existing test setup)

	if !ks.HasLabel("v1") || !ks.HasLabel("v2") {
		t.Fatal("expected v1 and v2 loaded")
	}
	if ks.HasLabel("v3") {
		t.Fatal("v3 must not be loaded")
	}
	if got := ks.LoadedLabels(); len(got) != 2 {
		t.Fatalf("LoadedLabels = %v, want 2", got)
	}
	if _, ok := ks.VersionID("v1"); !ok {
		t.Fatal("VersionID(v1) must resolve after Bootstrap")
	}
	aid, ok := ks.ActiveVersionID()
	if !ok {
		t.Fatal("ActiveVersionID must resolve")
	}
	vid, _ := ks.VersionID("v2")
	if aid != vid {
		t.Fatal("ActiveVersionID must equal VersionID(active)")
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`ks.HasLabel undefined`).

Run: `go test ./internal/envelope/ -run TestKeystoreAccessors`

- [ ] **Step 3: Implement the accessors** in `keystore.go` (read-only over the existing maps):

```go
// HasLabel reports whether this version's master key is loaded in memory.
func (k *Keystore) HasLabel(label string) bool {
	_, ok := k.masters[strings.ToLower(label)]
	return ok
}

// LoadedLabels returns every loaded version label (lowercased).
func (k *Keystore) LoadedLabels() []string {
	out := make([]string, 0, len(k.masters))
	for l := range k.masters {
		out = append(out, l)
	}
	return out
}

// VersionID returns the master_key_versions.id for a label, if cached
// (Bootstrap / loadVersions populates the cache).
func (k *Keystore) VersionID(label string) (uuid.UUID, bool) {
	id, ok := k.idByLabel[strings.ToLower(label)]
	return id, ok
}

// ActiveVersionID returns the active version's id.
func (k *Keystore) ActiveVersionID() (uuid.UUID, bool) {
	return k.VersionID(k.activeLabel)
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/envelope/ -run TestKeystoreAccessors`

```bash
gofmt -w internal/envelope/keystore.go internal/envelope/keystore_test.go
git add internal/envelope/keystore.go internal/envelope/keystore_test.go
git commit -m "feat(envelope): keystore label/version accessors for master-key rotation"
```

---

## Task 2: `internal/masterkey` — errors, Rotator, Start validation

**Files:**
- Create: `internal/masterkey/rotator.go`, `internal/masterkey/rotator_test.go`

- [ ] **Step 1: Write failing tests** (`dbtest` + a two-version keystore + seed `master_key_versions` rows). Assert the validation matrix and that a valid `Start` marks the source `rotating`.

```go
func TestStartValidation(t *testing.T) {
	pool := dbtest.NewPool(t)            // mirror existing dbtest usage
	ks := seedKeystoreV1V2Active2(t, pool) // v1+v2 loaded, active=v2, both version rows present
	r := masterkey.NewRotator(masterkey.Config{
		Q: gen.New(pool), Pool: pool, Keystore: ks, Concurrency: 2, BatchSize: 8, Logger: slog.Default(),
	})

	// to must equal the active label
	if err := r.Start(context.Background(), "v1", "v1"); !errors.Is(err, masterkey.ErrToNotActive) {
		t.Fatalf("to=v1 (not active) want ErrToNotActive, got %v", err)
	}
	// from must be loaded + non-active + have a row + not retired
	if err := r.Start(context.Background(), "v9", "v2"); !errors.Is(err, masterkey.ErrInvalidFrom) {
		t.Fatalf("unknown from want ErrInvalidFrom, got %v", err)
	}
	// happy path marks v1 rotating
	if err := r.Start(context.Background(), "v1", "v2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	row, _ := gen.New(pool).GetRotatingVersion(context.Background())
	if row.VersionLabel != "v1" {
		t.Fatalf("rotating version = %q, want v1", row.VersionLabel)
	}
	// a second concurrent rotation is refused
	if err := r.Start(context.Background(), "v1", "v2"); !errors.Is(err, masterkey.ErrAlreadyRotating) {
		t.Fatalf("second Start want ErrAlreadyRotating, got %v", err)
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`undefined: masterkey`).

Run: `go test ./internal/masterkey/`

- [ ] **Step 3: Implement `rotator.go`** (errors + struct + `Start`; drain/Run/Status added in later tasks). Use a buffered (size 1) trigger channel.

```go
// Package masterkey implements operator master-key rotation: re-wrapping every
// per-blob DEK and signing key from a retiring master-key version to the active
// one, online and in parallel, with no read-path downtime. See
// docs/specs/ENCRYPTION_ENVELOPE.md and the M10 design doc.
package masterkey

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

var (
	ErrToNotActive     = errors.New("masterkey: to_version must equal the active master-key label (set NOVA_MASTER_KEY_ACTIVE and restart first)")
	ErrInvalidFrom     = errors.New("masterkey: from_version must be a loaded, non-active, non-retired version")
	ErrAlreadyRotating = errors.New("masterkey: a rotation is already in progress")
)

// Config carries the Rotator's dependencies and tunables.
type Config struct {
	Q           *gen.Queries
	Pool        *pgxpool.Pool
	Keystore    *envelope.Keystore
	Audit       *auditlog.Writer // best-effort; nil ⇒ no audit
	Logger      *slog.Logger
	Concurrency int           // DEK worker goroutines; <=0 ⇒ 4
	BatchSize   int           // ids claimed per tx; <=0 ⇒ 256
	Pace        time.Duration // sleep between batch commits; <=0 ⇒ none
	Now         func() time.Time
	// OnSigningRewrap is invoked after signing keys are re-wrapped (the
	// coordinator wires it to signedurl KeySource.Invalidate). Defensive: re-wrap
	// does not change the secret value. Nil ⇒ skipped.
	OnSigningRewrap func()
}

type job struct{ from, to string }

// Rotator drives master-key rotation. Run it once via Run(ctx); trigger
// rotations via Start; observe via Status/Readyz.
type Rotator struct {
	q       *gen.Queries
	pool    *pgxpool.Pool
	ks      *envelope.Keystore
	audit   *auditlog.Writer
	log     *slog.Logger
	conc    int
	batch   int
	pace    time.Duration
	now     func() time.Time
	onSign  func()
	trigger chan job

	// test seam: invoked with the transient plaintext key right after re-wrap,
	// before zeroing, so a test can assert the buffer is subsequently zeroed.
	captureKey func([]byte)
}

func NewRotator(c Config) *Rotator {
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 256
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return &Rotator{
		q: c.Q, pool: c.Pool, ks: c.Keystore, audit: c.Audit, log: c.Logger,
		conc: c.Concurrency, batch: c.BatchSize, pace: c.Pace, now: c.Now,
		onSign: c.OnSigningRewrap, trigger: make(chan job, 1),
	}
}

// Start validates a rotation, atomically marks the source version 'rotating',
// and enqueues the drain (which Run executes). Non-blocking.
func (r *Rotator) Start(ctx context.Context, from, to string) error {
	if to != r.ks.ActiveLabel() || !r.ks.HasLabel(to) {
		return ErrToNotActive
	}
	if from == to || !r.ks.HasLabel(from) {
		return ErrInvalidFrom
	}
	row, err := r.q.GetMasterVersionByLabel(ctx, from)
	if err != nil || row.State == gen.KeyStateRetired {
		return ErrInvalidFrom
	}
	n, err := r.q.BeginVersionRotation(ctx, from)
	if err != nil {
		return fmt.Errorf("masterkey: begin rotation: %w", err)
	}
	if n == 0 {
		// Either another version is already rotating, or `from` was not 'active'.
		if _, e := r.q.GetRotatingVersion(ctx); e == nil {
			return ErrAlreadyRotating
		}
		return ErrInvalidFrom
	}
	// The handler writes the actor-attributed `master_key.rotation_started` audit
	// row (it holds the request context); the Rotator audits completed/resumed as
	// system actions (Task 4). Start only enqueues the drain.
	select {
	case r.trigger <- job{from, to}:
	default: // a drain is already queued/running; the DB 'rotating' guard is authoritative
	}
	return nil
}

// zero overwrites b with zeros (best-effort transient-secret hygiene).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

var _ = pgx.ErrNoRows // referenced in later tasks
var _ sync.WaitGroup
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/masterkey/ -run TestStartValidation`

```bash
gofmt -w internal/masterkey/
git add internal/masterkey/
git commit -m "feat(masterkey): Rotator + Start validation (to==active, from loaded, atomic begin)"
```

---

## Task 3: DEK re-wrap (claim, atomic guarded update, zeroing, pace)

**Files:**
- Modify: `internal/masterkey/rotator.go`, `internal/masterkey/rotator_test.go`

- [ ] **Step 1: Write failing tests.** Seed DEKs under v1: a normal blob DEK, a derivative DEK, a `legal_hold=true` DEK, a `shredded` DEK (zeroed wrapped_key), plus a `public_archival` blob with no DEK. Run `drainDEKs(ctx, "v1")` and assert every active/legal-hold/derivative DEK now references v2 and unwraps to its original plaintext; the shredded row is untouched; counts reach 0. Plus an idempotency + a zeroing assertion.

```go
func TestDrainDEKs(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV1V2Active2(t, pool)
	v1, _ := ks.VersionID("v1")
	v2, _ := ks.VersionID("v2")

	// Seed three v1 DEKs with known plaintext keys (wrap under v1 explicitly via
	// a helper that uses the v1 master key), incl. one legal_hold; plus a
	// shredded row (state='shredded', wrapped_key = 72 zero bytes).
	pbk := seedDEKUnderV1(t, pool, ks, false)          // plaintext key captured
	seedDEKUnderV1(t, pool, ks, true)                  // legal_hold
	shredID := seedShreddedDEK(t, pool, v1)

	r := newTestRotator(t, pool, ks)
	if err := r.drainDEKs(context.Background(), "v1"); err != nil {
		t.Fatalf("drainDEKs: %v", err)
	}

	// 0 active/rotating DEKs remain on v1.
	if n, _ := gen.New(pool).CountDEKsForVersion(context.Background(), v1); n != 0 {
		t.Fatalf("remaining v1 DEKs = %d, want 0", n)
	}
	// The first DEK now references v2 and unwraps to the same plaintext.
	got := unwrapDEKUnderKeystore(t, pool, ks, pbk.id) // looks up row, ks.Unwrap by recorded version
	if !bytes.Equal(got, pbk.key) {
		t.Fatal("re-wrapped DEK does not unwrap to the original plaintext")
	}
	// The shredded row keeps version v1 (untouched) and stays shredded.
	if v := dekVersion(t, pool, shredID); v != v1 {
		t.Fatal("shredded DEK must not be re-wrapped")
	}
	_ = v2
}

func TestDrainDEKsZeroesTransientKey(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV1V2Active2(t, pool)
	seedDEKUnderV1(t, pool, ks, false)

	r := newTestRotator(t, pool, ks)
	var captured []byte
	r.SetCaptureKeyForTest(func(b []byte) { captured = b }) // slice header saved; backing array shared
	if err := r.drainDEKs(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	for _, x := range captured {
		if x != 0 {
			t.Fatal("transient plaintext key was not zeroed after re-wrap")
		}
	}
}
```

- [ ] **Step 2: Run — expect FAIL** (`r.drainDEKs undefined`).

Run: `go test ./internal/masterkey/ -run TestDrainDEKs`

- [ ] **Step 3: Implement `drainDEKs` + workers + `drainBatch`** in `rotator.go`:

```go
// SetCaptureKeyForTest installs the zeroing test seam. Test-only.
func (r *Rotator) SetCaptureKeyForTest(fn func([]byte)) { r.captureKey = fn }

// drainDEKs re-wraps every active/rotating DEK for `from` to the active version,
// using r.conc parallel workers. Returns when the version is drained or ctx ends.
func (r *Rotator) drainDEKs(ctx context.Context, from string) error {
	fromID, ok := r.ks.VersionID(from)
	if !ok {
		return fmt.Errorf("masterkey: version id for %q not cached", from)
	}
	var wg sync.WaitGroup
	errc := make(chan error, r.conc)
	for i := 0; i < r.conc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				n, err := r.drainBatch(ctx, fromID)
				if err != nil {
					errc <- err
					return
				}
				if n == 0 {
					return
				}
				if r.pace > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(r.pace):
					}
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

// drainBatch claims and re-wraps up to r.batch DEKs in one transaction.
func (r *Rotator) drainBatch(ctx context.Context, fromID uuid.UUID) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := r.q.WithTx(tx)

	rows, err := q.ClaimDEKsForRewrap(ctx, gen.ClaimDEKsForRewrapParams{
		MasterKeyVersionID: fromID, Limit: int32(r.batch),
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	for _, row := range rows {
		pbk, err := r.ks.Unwrap(ctx, row.WrappedKey, fromID)
		if err != nil {
			return 0, fmt.Errorf("masterkey: unwrap dek %s: %w", row.ID, err)
		}
		wrapped, toID, err := r.ks.Wrap(pbk)
		if r.captureKey != nil {
			r.captureKey(pbk)
		}
		zero(pbk)
		if err != nil {
			return 0, fmt.Errorf("masterkey: wrap dek %s: %w", row.ID, err)
		}
		if _, err := q.RewrapDEK(ctx, gen.RewrapDEKParams{
			WrappedKey: wrapped, NewVersionID: toID, ID: row.ID, OldVersionID: fromID,
		}); err != nil {
			return 0, fmt.Errorf("masterkey: rewrap dek %s: %w", row.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/masterkey/ -run TestDrainDEKs`

```bash
gofmt -w internal/masterkey/
git add internal/masterkey/
git commit -m "feat(masterkey): parallel DEK re-wrap worker (claim, atomic guarded update, zeroing, pace)"
```

---

## Task 4: signing-key re-wrap, retire, drain orchestration, Run + resume

**Files:**
- Modify: `internal/masterkey/rotator.go`, `internal/masterkey/rotator_test.go`

- [ ] **Step 1: Write failing tests.** (a) Full drain: seed v1 DEKs + an active signing key + a within-grace retired signing key + a shredded signing key; run a rotation through `Run` (or call `drain` directly) → all DEKs + the active/retired signing keys on v2 (re-unwrap to original), shredded untouched, v1 `retired`, `OnSigningRewrap` invoked. (b) Resume: pre-mark v1 `rotating` with a partial drain, construct a Rotator, run `resumeIfRotating` → completes. (c) Stall: pre-mark a `rotating` version whose key is NOT loaded → `resumeIfRotating` returns without panic and leaves it `rotating`.

```go
func TestFullDrainRetiresAndRewrapsSigning(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV1V2Active2(t, pool)
	seedDEKUnderV1(t, pool, ks, false)
	sk := seedActiveSigningKeyUnderV1(t, pool, ks) // captured secret
	seedRetiredSigningKeyUnderV1(t, pool, ks, time.Now().Add(time.Hour))

	var invalidated bool
	r := newTestRotatorWith(t, pool, ks, func() { invalidated = true })

	if err := r.Start(context.Background(), "v1", "v2"); err != nil {
		t.Fatal(err)
	}
	r.drainOnceForTest(context.Background(), "v1", "v2") // synchronous drain wrapper

	v1, _ := ks.VersionID("v1")
	if n, _ := gen.New(pool).CountDEKsForVersion(context.Background(), v1); n != 0 {
		t.Fatalf("remaining v1 DEKs = %d", n)
	}
	if n, _ := gen.New(pool).CountSigningKeysForVersion(context.Background(), v1); n != 0 {
		t.Fatalf("remaining v1 signing keys = %d", n)
	}
	row, _ := gen.New(pool).GetMasterVersionByLabel(context.Background(), "v1")
	if row.State != gen.KeyStateRetired {
		t.Fatalf("v1 state = %v, want retired", row.State)
	}
	if got := unwrapSigningUnderKeystore(t, pool, ks, sk.kid); !bytes.Equal(got, sk.secret) {
		t.Fatal("re-wrapped signing key does not unwrap to original")
	}
	if !invalidated {
		t.Fatal("OnSigningRewrap not invoked")
	}
}

func TestResumeStallsWhenKeyMissing(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV2Only(t, pool)          // only v2 loaded; v1 row exists, marked rotating
	markVersionRotating(t, pool, "v1")
	r := newTestRotator(t, pool, ks)
	r.resumeIfRotating(context.Background())    // must not panic, must not retire v1
	row, _ := gen.New(pool).GetMasterVersionByLabel(context.Background(), "v1")
	if row.State != gen.KeyStateRotating {
		t.Fatal("stalled rotation must stay 'rotating'")
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./internal/masterkey/ -run 'TestFullDrain|TestResume'`

- [ ] **Step 3: Implement signing re-wrap + drain + Run + resume** in `rotator.go`:

```go
func (r *Rotator) rewrapSigningKeys(ctx context.Context, from string, fromID uuid.UUID) error {
	rows, err := r.q.ListSigningKeysForRewrap(ctx, fromID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		secret, err := r.ks.Unwrap(ctx, row.WrappedKey, fromID)
		if err != nil {
			return fmt.Errorf("masterkey: unwrap signing key %s: %w", row.Kid, err)
		}
		wrapped, toID, err := r.ks.Wrap(secret)
		zero(secret)
		if err != nil {
			return fmt.Errorf("masterkey: wrap signing key %s: %w", row.Kid, err)
		}
		if _, err := r.q.RewrapSigningKey(ctx, gen.RewrapSigningKeyParams{
			WrappedKey: wrapped, NewVersionID: toID, Kid: row.Kid, OldVersionID: fromID,
		}); err != nil {
			return fmt.Errorf("masterkey: rewrap signing key %s: %w", row.Kid, err)
		}
	}
	return nil
}

// drain runs a full rotation: DEKs → signing keys → retire. Stalls (returns,
// leaving 'rotating') if the source key is not loaded.
func (r *Rotator) drain(ctx context.Context, from, to string) {
	if !r.ks.HasLabel(from) {
		r.log.Warn("master-key rotation stalled: source key not loaded", "from", from)
		return // stays 'rotating'; Readyz degrades until the operator restores it
	}
	fromID, ok := r.ks.VersionID(from)
	if !ok {
		r.log.Error("master-key rotation: source version id not cached", "from", from)
		return
	}
	start := r.now()
	if err := r.drainDEKs(ctx, from); err != nil {
		r.log.Error("master-key rotation: drain DEKs", "from", from, "err", err)
		return
	}
	if err := r.rewrapSigningKeys(ctx, from, fromID); err != nil {
		r.log.Error("master-key rotation: rewrap signing keys", "from", from, "err", err)
		return
	}
	if err := r.q.RetireVersion(ctx, from); err != nil {
		r.log.Error("master-key rotation: retire version", "from", from, "err", err)
		return
	}
	if r.onSign != nil {
		r.onSign()
	}
	r.log.Info("master-key rotation completed", "from", from, "to", to, "duration", r.now().Sub(start))
	if r.audit != nil {
		r.audit.Write(ctx, auditlog.Entry{
			Action: "master_key.rotation_completed", TargetType: "master_key_version", TargetID: from,
			Payload: map[string]any{"from": from, "to": to, "duration_ms": r.now().Sub(start).Milliseconds()},
		})
	}
}

// Run drives the Rotator: it resumes any interrupted rotation, then drains on
// each trigger until ctx is cancelled. Start exactly once (coordinator.Run).
func (r *Rotator) Run(ctx context.Context) {
	r.resumeIfRotating(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-r.trigger:
			r.drain(ctx, j.from, j.to)
		}
	}
}

// resumeIfRotating finishes a rotation interrupted by a restart. The target is
// the current active version (the operator restarted with NOVA_MASTER_KEY_ACTIVE
// already flipped). No-op when nothing is rotating or the source == active.
func (r *Rotator) resumeIfRotating(ctx context.Context) {
	row, err := r.q.GetRotatingVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		r.log.Error("master-key rotation: resume probe", "err", err)
		return
	}
	from, to := row.VersionLabel, r.ks.ActiveLabel()
	if from == to {
		r.log.Warn("master-key rotation: rotating version equals active; skipping resume", "version", from)
		return
	}
	if !r.ks.HasLabel(from) {
		r.log.Warn("master-key rotation stalled on resume: source key not loaded", "from", from)
		return
	}
	if r.audit != nil {
		r.audit.Write(ctx, auditlog.Entry{
			Action: "master_key.rotation_resumed", TargetType: "master_key_version", TargetID: from,
			Payload: map[string]any{"from": from, "to": to},
		})
	}
	r.drain(ctx, from, to)
}

// drainOnceForTest runs a single synchronous drain. Test-only.
func (r *Rotator) drainOnceForTest(ctx context.Context, from, to string) { r.drain(ctx, from, to) }
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/masterkey/ -run 'TestFullDrain|TestResume'`

```bash
gofmt -w internal/masterkey/
git add internal/masterkey/
git commit -m "feat(masterkey): signing-key re-wrap + retire + Run loop + resume-on-boot"
```

---

## Task 5: Status + Readyz (stall detection)

**Files:**
- Modify: `internal/masterkey/rotator.go`, `internal/masterkey/rotator_test.go`

- [ ] **Step 1: Write failing tests.** Idle (no rotation) → `Status.InProgress == nil`, `Readyz == nil`. Rotating with key loaded → `InProgress` populated, `stalled=false`, `Readyz == nil`. Rotating with key NOT loaded → `stalled=true`, `Readyz` returns an error.

```go
func TestStatusAndReadyz(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV1V2Active2(t, pool)
	r := newTestRotator(t, pool, ks)

	st, _ := r.Status(context.Background())
	if st.InProgress != nil {
		t.Fatal("idle: InProgress must be nil")
	}
	if err := r.Readyz(context.Background()); err != nil {
		t.Fatalf("idle Readyz: %v", err)
	}

	seedDEKUnderV1(t, pool, ks, false)
	markVersionRotating(t, pool, "v1")
	st, _ = r.Status(context.Background())
	if st.InProgress == nil || st.InProgress.From != "v1" || st.InProgress.Stalled {
		t.Fatalf("rotating(loaded): %+v", st.InProgress)
	}
	if err := r.Readyz(context.Background()); err != nil {
		t.Fatalf("loaded Readyz must pass: %v", err)
	}

	ks2 := seedKeystoreV2Only(t, pool) // v1 not loaded
	r2 := newTestRotator(t, pool, ks2)
	st, _ = r2.Status(context.Background())
	if st.InProgress == nil || !st.InProgress.Stalled {
		t.Fatal("rotating(unloaded): Stalled must be true")
	}
	if err := r2.Readyz(context.Background()); err == nil {
		t.Fatal("unloaded Readyz must degrade")
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./internal/masterkey/ -run TestStatusAndReadyz`

- [ ] **Step 3: Implement `Status` + `Readyz`** in `rotator.go`:

```go
type VersionInfo struct {
	Label        string     `json:"label"`
	State        string     `json:"state"`
	DEKCount     int64      `json:"dek_count"`
	SigningCount int64      `json:"signing_count"`
	RetiredAt    *time.Time `json:"retired_at"`
}

type Progress struct {
	From                 string    `json:"from"`
	RemainingDEKs        int64     `json:"remaining_deks"`
	RemainingSigningKeys int64     `json:"remaining_signing_keys"`
	Stalled              bool      `json:"stalled"`
	StallReason          string    `json:"stall_reason,omitempty"`
}

type Status struct {
	Active     string        `json:"active"`
	InProgress *Progress     `json:"in_progress"`
	Versions   []VersionInfo `json:"versions"`
}

func (r *Rotator) Status(ctx context.Context) (Status, error) {
	out := Status{Active: r.ks.ActiveLabel()}
	rows, err := r.q.ListMasterVersions(ctx)
	if err != nil {
		return out, err
	}
	for _, v := range rows {
		vi := VersionInfo{Label: v.VersionLabel, State: string(v.State), DEKCount: v.DekCount, SigningCount: v.SigningCount}
		if v.RetiredAt.Valid {
			t := v.RetiredAt.Time
			vi.RetiredAt = &t
		}
		out.Versions = append(out.Versions, vi)
		if v.State == gen.KeyStateRotating {
			p := &Progress{From: v.VersionLabel, RemainingDEKs: v.DekCount, RemainingSigningKeys: v.SigningCount}
			if !r.ks.HasLabel(v.VersionLabel) {
				p.Stalled = true
				p.StallReason = "source master key not loaded"
			}
			out.InProgress = p
		}
	}
	return out, nil
}

// Readyz degrades when a rotation is stalled (a 'rotating' version whose key is
// not loaded). Wire as a /readyz ReadyCheck — NOT a liveness probe; a restart
// cannot fix a missing key.
func (r *Rotator) Readyz(ctx context.Context) error {
	row, err := r.q.GetRotatingVersion(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return nil // a transient query error is not a rotation stall
	}
	if !r.ks.HasLabel(row.VersionLabel) {
		return fmt.Errorf("master-key rotation stalled: version %q key not loaded", row.VersionLabel)
	}
	return nil
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/masterkey/`

```bash
gofmt -w internal/masterkey/
git add internal/masterkey/
git commit -m "feat(masterkey): rotation Status projection + Readyz stall detection"
```

---

## Task 6: admin handler (rotate-master + rotation-status)

**Files:**
- Create: `internal/api/handlers/masterkey_admin.go`, `internal/api/handlers/masterkey_admin_test.go`

- [ ] **Step 1: Write failing tests** (handler-level over a real `Rotator` on `dbtest`): `POST /rotate-master {from:v1,to:v2}` → `202` and v1 marked rotating; `to != active` → `400 to_not_active`; second call while rotating → `409 rotation_in_progress`; `GET /rotation-status` → `200` with the JSON shape. (Authz is covered in the server/integration tasks.)

```go
func TestMasterKeyAdmin(t *testing.T) {
	pool := dbtest.NewPool(t)
	ks := seedKeystoreV1V2Active2(t, pool)
	r := masterkey.NewRotator(masterkey.Config{Q: gen.New(pool), Pool: pool, Keystore: ks, Logger: slog.Default()})
	h := handlers.NewMasterKeyAdminHandler(r, nil) // nil audit writer in the unit test

	rec := httptest.NewRecorder()
	h.RotateMaster(rec, postJSON(t, `{"from_version":"v1","to_version":"v2"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rotate-master = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.RotateMaster(rec, postJSON(t, `{"from_version":"v1","to_version":"v1"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("to!=active = %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.RotationStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./internal/api/handlers/ -run TestMasterKeyAdmin`

- [ ] **Step 3: Implement `masterkey_admin.go`** (map domain errors → status, mirroring `signing_admin.go`):

```go
package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/masterkey"
)

// MasterKeyAdminHandler serves the M10 master-key rotation endpoints:
//
//	POST /api/v1/admin/keys/rotate-master   (operator)
//	GET  /api/v1/admin/keys/rotation-status (operator)
type MasterKeyAdminHandler struct {
	r     *masterkey.Rotator
	audit *auditlog.Writer // best-effort; nil ⇒ no audit
}

func NewMasterKeyAdminHandler(r *masterkey.Rotator, audit *auditlog.Writer) *MasterKeyAdminHandler {
	return &MasterKeyAdminHandler{r: r, audit: audit}
}

type rotateMasterRequest struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
}

func (h *MasterKeyAdminHandler) RotateMaster(w http.ResponseWriter, req *http.Request) {
	rid := middleware.RequestIDFromContext(req.Context())
	ctx := req.Context()
	var body rotateMasterRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
		return
	}
	err := h.r.Start(ctx, body.FromVersion, body.ToVersion)
	switch {
	case err == nil:
		// fallthrough to 202
	case errors.Is(err, masterkey.ErrToNotActive):
		httputil.WriteError(w, http.StatusBadRequest, "to_not_active", err.Error(), rid)
		return
	case errors.Is(err, masterkey.ErrInvalidFrom):
		httputil.WriteError(w, http.StatusBadRequest, "invalid_from_version", err.Error(), rid)
		return
	case errors.Is(err, masterkey.ErrAlreadyRotating):
		httputil.WriteError(w, http.StatusConflict, "rotation_in_progress", err.Error(), rid)
		return
	default:
		slog.Error("rotate-master", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotation failed", rid)
		return
	}
	if h.audit != nil {
		h.audit.Write(ctx, auditlog.Entry{
			ActorID: ownerFromContext(ctx), Action: "master_key.rotation_started",
			TargetType: "master_key_version", TargetID: body.FromVersion,
			Payload: map[string]any{"from": body.FromVersion, "to": body.ToVersion},
		})
	}
	st, _ := h.r.Status(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"from": body.FromVersion, "to": body.ToVersion, "status": st,
	})
}

func (h *MasterKeyAdminHandler) RotationStatus(w http.ResponseWriter, req *http.Request) {
	rid := middleware.RequestIDFromContext(req.Context())
	st, err := h.r.Status(req.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "status failed", rid)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
```

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./internal/api/handlers/ -run TestMasterKeyAdmin`

```bash
gofmt -w internal/api/handlers/masterkey_admin.go internal/api/handlers/masterkey_admin_test.go
git add internal/api/handlers/masterkey_admin.go internal/api/handlers/masterkey_admin_test.go
git commit -m "feat(api): master-key rotate + rotation-status handlers"
```

---

## Task 7: mount routes + ServerConfig + coordinator/cmd wiring

**Files:**
- Modify: `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`, `internal/config/types.go`

- [ ] **Step 1: ServerConfig + mount (operator-only).** In `internal/api/server.go`, add the nil-able field beside `SigningAdmin`:

```go
MasterKeyAdmin *handlers.MasterKeyAdminHandler // M10; nil ⇒ unmounted
```

Inside the `/api/v1/admin` group (already `RequireRole("operator","moderator")`), mount operator-only, mirroring `rotate-signing`:

```go
if cfg.MasterKeyAdmin != nil {
	r.With(bearer.RequireRole("operator")).Post("/keys/rotate-master", cfg.MasterKeyAdmin.RotateMaster)
	r.With(bearer.RequireRole("operator")).Get("/keys/rotation-status", cfg.MasterKeyAdmin.RotationStatus)
}
```

- [ ] **Step 2: Config type.** In `internal/config/types.go`:

```go
// MasterKeyRotation tunes the M10 re-wrap worker. Zero-valued fields take the
// documented defaults (4 workers, 256 ids/batch, 50ms inter-batch pace).
type MasterKeyRotation struct {
	RewrapConcurrency int           `yaml:"rewrap_concurrency"`
	RewrapBatchSize   int           `yaml:"rewrap_batch_size"`
	RewrapPace        time.Duration `yaml:"rewrap_pace"`
}
```

- [ ] **Step 3: coordinator wiring.** In `pkg/coordinator/coordinator.go`: add `MasterKeyRotation MasterKeyRotationConfig` to `Config` (a coordinator-local struct mirroring the config type: `RewrapConcurrency int; RewrapBatchSize int; RewrapPace time.Duration`), a `masterKeyRotator *masterkey.Rotator` field, and build it in the `pool != nil && ks != nil` block beside the signed-URL stack:

```go
mkr := masterkey.NewRotator(masterkey.Config{
	Q: gen.New(pool), Pool: pool, Keystore: ks, Audit: auditW, Logger: slog.Default(),
	Concurrency: cfg.MasterKeyRotation.RewrapConcurrency,
	BatchSize:   cfg.MasterKeyRotation.RewrapBatchSize,
	Pace:        cfg.MasterKeyRotation.RewrapPace,
	OnSigningRewrap: func() {
		if c.revocations != nil { /* keySource lives in SigningAdmin; expose Invalidate */ }
	},
})
c.masterKeyRotator = mkr
sc.MasterKeyAdmin = handlers.NewMasterKeyAdminHandler(mkr, auditW)
```

Wire `OnSigningRewrap` to the M7 key-source `Invalidate`: the `DBKeySource` is constructed locally as `keySource` in the same block — capture it (`ks2 := keySource; OnSigningRewrap: func(){ ks2.Invalidate() }`). Register the readiness check beside `signing_keys`:

```go
if c.masterKeyRotator != nil {
	mkr := c.masterKeyRotator
	ready = append(ready, handlers.ReadyCheck{
		Name: "master_key_rotation",
		Fn:   func(ctx context.Context) error { return mkr.Readyz(ctx) },
	})
}
```

Start it in `Run` beside the others:

```go
if c.masterKeyRotator != nil {
	go c.masterKeyRotator.Run(ctx)
}
```

- [ ] **Step 4: cmd env knobs.** In `cmd/coordinator/main.go`, populate the config (beside `Moderation`):

```go
MasterKeyRotation: coordinator.MasterKeyRotationConfig{
	RewrapConcurrency: envInt("NOVA_MASTER_KEY_REWRAP_CONCURRENCY", 4),
	RewrapBatchSize:   envInt("NOVA_MASTER_KEY_REWRAP_BATCH", 256),
	RewrapPace:        time.Duration(envInt("NOVA_MASTER_KEY_REWRAP_PACE_MS", 50)) * time.Millisecond,
},
```

Document the three vars in the header comment block (beside `NOVA_MODERATION_SWEEP_ENABLED`).

- [ ] **Step 5: Build + unit; gofmt; commit.**

Run: `go build ./... && go test ./internal/api/... ./pkg/coordinator/... -short`
Expected: PASS.

```bash
gofmt -w internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go internal/config/types.go
git add internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go internal/config/types.go
git commit -m "feat(coordinator,api,cmd): wire master-key Rotator + routes + readyz + env knobs"
```

---

## Task 8: `novactl keys` subcommand group

**Files:**
- Modify: `cmd/novactl/main.go`, `cmd/novactl/main_test.go`

- [ ] **Step 1: Write a failing test** for argument parsing / dispatch (mirror the existing `cmdModeration*` tests): `keys` with no subcommand prints usage and errors; `keys rotate-master` without `--to` errors.

```go
func TestKeysDispatch(t *testing.T) {
	if err := cmdKeys([]string{}); err == nil {
		t.Fatal("keys with no subcommand must error")
	}
	if err := cmdKeys([]string{"rotate-master", "--from", "v1"}); err == nil {
		t.Fatal("rotate-master without --to must error")
	}
}
```

- [ ] **Step 2: Run — expect FAIL.**

Run: `go test ./cmd/novactl/ -run TestKeysDispatch`

- [ ] **Step 3: Implement the `keys` group.** Add `cmdKeys`, `cmdKeysRotateMaster` (POST then poll), `cmdKeysStatus` (GET), a `case "keys": return cmdKeys(os.Args[2:])` in `main()`, and `usage()` lines. `rotate-master` confirms unless `--no-confirm`, then polls `rotation-status` every ~2s printing progress until `in_progress` is null (or prints `stalled` and exits non-zero).

```go
func cmdKeys(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: novactl keys <rotate-master|status> ...")
	}
	switch args[0] {
	case "rotate-master":
		return cmdKeysRotateMaster(args[1:])
	case "status":
		return cmdKeysStatus(args[1:])
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

func cmdKeysRotateMaster(args []string) error {
	fs := flag.NewFlagSet("rotate-master", flag.ContinueOnError)
	from := fs.String("from", "", "retiring version label (e.g. v1)")
	to := fs.String("to", "", "new active version label (e.g. v2)")
	noConfirm := fs.Bool("no-confirm", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" {
		return errors.New("rotate-master requires --from and --to")
	}
	c, err := loadCreds()
	if err != nil {
		return err
	}
	if !*noConfirm && !confirm(fmt.Sprintf("Re-wrap every key from %q to %q? This re-encrypts all DEKs and signing keys. [y/N] ", *from, *to)) {
		return errors.New("aborted")
	}
	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/keys/rotate-master",
		map[string]any{"from_version": *from, "to_version": *to}, c.AccessToken)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusAccepted {
		return httpError("rotate-master", resp)
	}
	resp.Body.Close()
	fmt.Printf("rotation started: %s → %s; polling...\n", *from, *to)
	return pollRotation(c)
}

// pollRotation prints progress until the rotation clears or stalls.
func pollRotation(c credentials) error {
	for {
		st, err := fetchRotationStatus(c)
		if err != nil {
			return err
		}
		if st.InProgress == nil {
			fmt.Println("rotation complete.")
			return nil
		}
		if st.InProgress.Stalled {
			return fmt.Errorf("rotation STALLED: %s (restore the %q master key and restart the coordinator)",
				st.InProgress.StallReason, st.InProgress.From)
		}
		fmt.Printf("  remaining: %d DEKs, %d signing keys\n", st.InProgress.RemainingDEKs, st.InProgress.RemainingSigningKeys)
		time.Sleep(2 * time.Second)
	}
}
```

(Add `cmdKeysStatus` printing the version table from `fetchRotationStatus`, plus a `fetchRotationStatus` helper and a `rotationStatus` response struct mirroring `masterkey.Status`. Reuse the **existing** `confirm` (`cmd/novactl/main.go:470`, already used by `moderation takedown`), `newClient`/`postJSON`/`getJSON`/`loadCreds`, and `httpError`.)

- [ ] **Step 4: Run — expect PASS; gofmt; commit.**

Run: `go test ./cmd/novactl/ -run TestKeysDispatch`

```bash
gofmt -w cmd/novactl/main.go cmd/novactl/main_test.go
git add cmd/novactl/main.go cmd/novactl/main_test.go
git commit -m "feat(novactl): keys subcommand (rotate-master with progress poll + status)"
```

---

## Task 9: integration test (two-boot, nginx-fronted)

**Files:**
- Create: `internal/integration/m10_master_key_rotation_test.go`

- [ ] **Step 1: Write the integration test** (model on `m9_moderation_test.go` for the nginx + testcontainers + boot harness). Two boots sharing one Postgres:

```
Boot #1 (NOVA_MASTER_KEY_ACTIVE=v1, only v1):
  - create operator + moderator + uploader (helpers from the M9 test)
  - upload an encrypted blob (+ derivative), and (M9) quarantine one with --legal-hold
  - mint a signed URL for the blob (under v1's signing key)
  - shut the coordinator down (keep Postgres)

Boot #2 (v1 + v2 loaded, NOVA_MASTER_KEY_ACTIVE=v2, small batch + pace):
  - sanity: GET the blob → 200 (reads under either version)
  - POST /api/v1/admin/keys/rotate-master {from:v1,to:v2} as operator → 202
  - while in_progress, GET an un-migrated blob → 200 (Exit #3, either-version read)
  - poll /rotation-status to completion
  - assert: CountDEKsForVersion(v1)=0, CountSigningKeysForVersion(v1)=0, v1 state=retired (Exit #1,#2)
  - the signed URL still verifies → 200 (Exit #2)
  - GET /api/v1/admin/audit-log shows master_key.rotation_started then _completed
  - authz: rotate-master with moderator token → 403; no token → 401
```

For the crash-resume leg (Exit #4): start a rotation, cancel boot #2's context mid-drain (small batch + a pause), start boot #3 with the same env → `ResumeIfRotating` finishes it; assert v1 retired.

- [ ] **Step 2: Run — expect FAIL** (compile/wiring), then iterate to green.

Run: `go test ./internal/integration/ -run M10`

- [ ] **Step 3: gofmt; commit.**

```bash
gofmt -w internal/integration/m10_master_key_rotation_test.go
git add internal/integration/m10_master_key_rotation_test.go
git commit -m "test(m10): two-boot rotation e2e (drain, either-version read, signing continuity, resume) through nginx"
```

---

## Task 10: doc reconciliations + roadmap

**Files:**
- Modify: `docs/specs/ENCRYPTION_ENVELOPE.md`, `docs/specs/DATA_MODEL.sql`, `docs/specs/openapi.yaml`, `docs/legal/OPERATOR_CHECKLIST.md`, `docs/THREAT_MODEL.md`, `docs/ROADMAP.md`

- [ ] **Step 1: `ENCRYPTION_ENVELOPE.md` reconciliation #1.** In § "Rotation procedure (Phase 1 deliverable)": replace the per-row `state='rotating'`/`'active'` steps with the single atomic version-guarded `UPDATE` (state stays `active`; `rotating` marks the version row); replace the `novactl … --new-key-env …` block with `novactl keys rotate-master --from v1 --to v2`; add the restart-to-flip-active precondition and the `to == active label` invariant; change "(Phase 1 deliverable)" / "(realised in M10)" to "implemented in M10 (`internal/masterkey`)".

- [ ] **Step 2: `DATA_MODEL.sql` reconciliation #2.** On `master_key_versions` / `data_encryption_keys`: note `rotating` marks the in-progress source version; `dek_master_version_idx` is the re-wrap worker's claim index; a version row is never deleted (FK from shredded rows), only `retired`; re-wrap is an atomic guarded UPDATE, not a shred (legal-hold rows are re-wrapped).

- [ ] **Step 3: `openapi.yaml` reconciliation #3.** Add `POST /api/v1/admin/keys/rotate-master` (`RotateMasterRequest{from_version,to_version}` → `202`; `409 rotation_in_progress`; `400`) and `GET /api/v1/admin/keys/rotation-status` (`200` → the `Status` schema: `active`, nullable `in_progress`, `versions[]`). Document operator-only `401`/`403`.

- [ ] **Step 4: `OPERATOR_CHECKLIST.md` reconciliation #4.** Add the five-step rotation runbook; the "back up **every** MK version out-of-band; loss of all = permanent loss of every blob" mandate; the "do not drop the old key until `novactl keys status` shows it `retired` with 0 rows — otherwise the rotation stalls and `/readyz` degrades" caution; and autovacuum guidance for `data_encryption_keys` on large deployments (the re-wrap generates dead tuples; consider a more aggressive table-level `autovacuum_vacuum_scale_factor` during a large rotation).

- [ ] **Step 5: `THREAT_MODEL.md` reconciliation #6.** Boundary ③: one line noting both MK versions are process-resident during a rotation window.

- [ ] **Step 6: `ROADMAP.md` reconciliation #5.** Mark M10 ✅; link this design + plan; note the `m10-master-key-rotation` tag; record deferrals (runtime activation → not planned; generator → later; cross-node → Phase 2; `rotate-signing` CLI wrapper → optional).

- [ ] **Step 7: Full verification + commit.**

Run: `make sqlc-generate && make codegen-check && go build ./... && go test ./... -short`
Expected: PASS. Then the integration job: `go test ./internal/integration/ -run M10`.

```bash
git add docs/
git commit -m "docs(m10): reconcile ENCRYPTION_ENVELOPE rotation, DATA_MODEL, openapi, operator runbook, threat-model, roadmap"
```

---

## Self-review notes (for the executor)

- **Spec coverage:** Task 0 = queries; Task 1 = keystore accessors; Tasks 2–5 = the Rotator (validation, DEK re-wrap, signing re-wrap + retire + resume, Status/Readyz); Task 6 = handlers; Task 7 = wiring + readyz + config; Task 8 = CLI; Task 9 = integration (all four exit criteria); Task 10 = the six doc reconciliations. Every design section maps to a task.
- **Type consistency:** `Status`/`Progress`/`VersionInfo` are defined once (Task 5) and reused by the handler (Task 6) and CLI (Task 8). `masterkey.Config` fields (`Concurrency`/`BatchSize`/`Pace`/`OnSigningRewrap`/`Now`) are fixed in Task 2 and consumed in Task 7. The two re-wrap queries use sqlc named args (`new_version_id`/`old_version_id`) → `RewrapDEKParams{WrappedKey, NewVersionID, ID, OldVersionID}`.
- **Helper note:** the test helpers (`seedKeystoreV1V2Active2`, `seedDEKUnderV1`, `seedActiveSigningKeyUnderV1`, `unwrapDEKUnderKeystore`, `markVersionRotating`, etc.) are introduced in Task 2/3's test files; reuse the existing `internal/dbtest`/`internal/blobfixture`/`internal/envelope` test setup wherever one already exists rather than duplicating.
- **`OnSigningRewrap` wiring (Task 7):** capture the M7 `keySource` local in `coordinator.New` and pass `func(){ keySource.Invalidate() }`; do not import `signedurl` into `internal/masterkey` (keep it a leaf).
```
