# Phase 1 M8 — Integrity-Audit Scheduler Design

## Purpose and scope

M8 is the eighth Phase-1 milestone and the third of the backend-capability band
(M6–M10). It is Nova's **proof-of-correctness backbone**: a coordinator-internal
audit loop that catches implementation bugs and silent corruption in the local
Postgres state, the local Kubo blockstore, and the local keystore — *before* donors
are ever exposed to the data (Phase 2). There are no donor messages, no challenge
tokens, and no remote calls; everything runs against state the coordinator already
owns.

The behaviour is already **normative**: `docs/specs/INTEGRITY_AUDIT.md` (Status:
"Phase 1 deliverable, normative; `internal/audit/integrity` must conform exactly")
defines the seven `audit_kind` invariants, their per-kind schedule and sample sizes,
the reporting contract, and the retention policy. M8 *implements that spec* —
drift between the two is a bug in the implementation. Three pieces of groundwork
already ship and M8 consumes them unchanged:

- **Schema.** `integrity_audits` and the `audit_kind` / `audit_result` enums ship in
  `internal/db/migrations/0001_init.sql` and are converted to monthly RANGE partitions
  in `0003_partitions.sql`. **M8 adds no new table and no migration.**
- **HTTP contract.** `GET /api/v1/admin/audits/integrity` (`operationId:
  listIntegrityAudits`) and the `IntegrityAudit` / `PaginatedIntegrityAudits` schemas
  are specified in `docs/specs/openapi.yaml:1252,2037,2101`.
- **Check primitives.** Every verification the seven kinds need already exists:
  `envelope.Decode` (`internal/envelope/envelope.go:66`), `keystore.Unwrap`
  (`internal/envelope/keystore.go:221`), the decrypt pipeline modelled by
  `storage.OpenBytes` (`pkg/coordinator/storage/blob.go:171`), and the IPFS
  `Backend.{Has,BlockstoreHas,BlockGet,Get}` methods (`internal/ipfs/backend.go:72-98`)
  — whose doc comments already name `block_hash_valid` as their consumer.

The package path `internal/audit/integrity` is a `.gitkeep` stub today; M8 populates it.

### In scope

- **`internal/audit/integrity`**: the audit subsystem — a `Scheduler` (in-process,
  per-kind cadence), the seven `Check`s, a `Recorder` (batch insert + failure
  surfacing), a `FailureSink` seam, a partition/retention `Maintainer`, and the
  cadence config + spec-default constants.
- **The seven audit kinds**, each conforming to `INTEGRITY_AUDIT.md`:
  `envelope_decode`, `key_unwrap`, `sample_decrypt`, `kubo_pin_present`,
  `derivative_state_consistent`, `block_hash_valid`, `manifest_consistent`.
- **`internal/db/queries/audit.sql`** (sqlc): row sampling, batched audit-row insert,
  the paginated listing + count, and the per-kind `MAX(audited_at)` schedule seed.
- **Failure surfacing**: structured `slog.Warn` per failure + the durable
  `integrity_audits` rows + the admin listing endpoint.
- **`GET /api/v1/admin/audits/integrity`**: paginated, filterable
  (`result`, `audit_kind`), `RequireRole("operator","moderator")`. Introduces the
  repo's first pagination helper (`internal/api/httputil/pagination.go`).
- **Partition lifecycle**: monthly create-ahead (so inserts never hit an
  uncovered range) + retention pruning (passes 30 d, failures ≥ 1 y).
- **Wiring**: an `IntegrityAudit` section on `coordinator.Config`; build the
  `Scheduler` + `Maintainer` in `coordinator.New`; start them in `Run`; populate
  spec defaults in `cmd/coordinator`.
- Unit tests + an nginx-fronted integration test; CI exercises the untagged binary.

### Out of scope (with the milestone that owns each)

- **The `nova_integrity_audit_failures_total` metric** — **deferred to a future
  observability milestone.** The repo has no metrics surface at all (no
  Prometheus/expvar/`/metrics`). Standing up a scrape endpoint for a single counter
  is poorly scoped and cross-cutting (jobs, uploads, and auth would all want metrics
  together). The roadmap's "failure surfacing" is satisfied by warn logs + the
  `integrity_audits` rows + the admin endpoint. Reconciliation #1 records the
  deferral. (Precedent: M7 deferred the `audit_log` writer to M9 and pubsub to Phase 2.)
- **The `integrity.audit_failed` outbound webhook** — **deferred.** `internal/webhook`
  is a `.gitkeep` stub and the spec marks the webhook off-by-default. M8 ships the
  **seam** — a `FailureSink` interface the Recorder calls on every failure, with a
  log-only default — so the delivery subsystem can land later (with the milestone that
  owns outbound notifications) without touching the audit core.
- **Auto-remediation / one-click "remediate"** — **M9 + M11.** `INTEGRITY_AUDIT.md`
  § "Failure handling" is explicit that the audit *reports* and never remediates; the
  remediation actions and the admin "recent failures" panel are admin-SPA + moderation
  surface (M11 consumes this endpoint; M9 adds the state transitions that
  `derivative_state_consistent` exists to police).
- **`novactl audits run-now`** — not shipped, but the `Scheduler.RunKind` /
  `RunOnce` entry points that a future CLI/admin "run now" would call are built (they
  are also how the tests drive audits deterministically).
- **`operator.yaml` wiring** — still deferred (M5–M7 precedent). The
  `config.IntegrityAudit` / `AuditCadence` structs already exist
  (`internal/config/types.go:97-110`); M8 ships the same defaults as code constants on
  `coordinator.Config` and leaves the YAML loader hookup to the setup-wizard milestone.

## Source of truth and required doc reconciliations

1. **`docs/specs/INTEGRITY_AUDIT.md` — record the two deferrals and tighten three
   descriptions to the v1 reality.**
   - § "Reporting": note the `nova_integrity_audit_failures_total` metric is **deferred
     to a future observability milestone**; M8 surfaces failures via warn logs, the
     `integrity_audits` rows, and `GET /api/v1/admin/audits/integrity`.
   - § "Reporting": note the `integrity.audit_failed` webhook is **deferred**; M8 ships
     a `FailureSink` seam (log-only default).
   - § "Scope" `sample_decrypt` row: the "small random byte range" framing presumes the
     Phase-2 streaming-AEAD codec. v1 is single-shot XChaCha20-Poly1305 — the tag covers
     the whole envelope, so the check **decrypts the whole sampled envelope** (bounded by
     a size cap + the small sample). Clarify this in place.
   - § "Schedule" `derivative_state_consistent` row: "every derivative whose parent state
     changed in the past hour" presumes a state-change timestamp that does not exist in
     Phase 1 (`blobs` has only `uploaded_at`). M8 **samples derivatives and compares each
     to its parent's current state**; the precise change-window filter is deferred until
     M9 introduces state-transition tracking. Until M9 adds quarantine/tombstone cascades,
     this check has nothing to fail — it exists to police those cascades.
   - § "Performance considerations" / retention: make the mechanism concrete — monthly
     partition create-ahead, `DELETE` of `pass` rows older than 30 d, and `DROP` of
     partitions entirely older than the failure-retention window (default ≥ 1 y).

2. **`docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` — reconcile the
   execution model to the normative spec.** The "Integrity audit loop" sketch (lines
   ~511–530) and the M8 milestone bullet (~929–934) describe the scheduler **enqueuing
   `jobs.integrity_audit_run` jobs for the worker pool**. `INTEGRITY_AUDIT.md` is normative
   and requires a "background goroutine" with "**no persistent in-flight queue**" that
   "resume[s] from the schedule's natural cadence" on restart. The sketch is reconciled to
   the **in-process** model M8 implements (the read-heavy verification loop is deliberately
   isolated from the write-heavy M2 job system); the spec is not loosened to fit the sketch.

3. **`docs/specs/openapi.yaml` — confirm and complete `listIntegrityAudits`.** The path,
   the `result`/`audit_kind` query filters, and the `IntegrityAudit` /
   `PaginatedIntegrityAudits` schemas already match the planned handler. Confirm the shared
   `PageParam` / `PerPageParam` parameter components exist (referenced at `:1262-1263`) and
   add them if absent; document the `401 unauthenticated` / `403 forbidden` admin responses.

4. **`docs/ROADMAP.md` + the master plan — M8 row.** Mark M8 status and link this design +
   its implementation plan. Record the `m8-integrity-audit` tag on completion.

5. **`internal/config/types.go`** — no struct change (the `IntegrityAudit` /
   `AuditCadence` shapes already mirror this design). Add a comment that M8 consumes the
   defaults via `coordinator.Config` and that `operator.yaml` decode stays deferred.

## Preconditions from M1–M7 (confirmed in committed code)

- **Schema present.** `audit_kind` enum = the seven kinds (`DATA_MODEL.sql:138`);
  `audit_result` enum = `pass | fail | skip` (`:148`). `integrity_audits (id bigserial,
  cid text NOT NULL, audit_kind, result, error text, audited_at timestamptz)` is
  RANGE-partitioned by `audited_at` (`0003_partitions.sql`), with partitions
  `integrity_audits_default` (→ 2026-06-01) and `integrity_audits_2026_06`
  (2026-06-01 → **2026-07-01**). The partial index `integrity_audits_failures_idx … WHERE
  result <> 'pass'` already serves the failures listing.
- **Check primitives present.**
  - `envelope.Decode(b) (version, Codec, err)` validates magic/version/algo/reserved and
    needs only the 32-byte `HeaderSize` prefix (`internal/envelope/envelope.go:59-92`).
  - `Keystore.Unwrap(ctx, wrapped, versionID) ([]byte, error)`
    (`internal/envelope/keystore.go:221`).
  - The decrypt sequence `backend.Get → io.ReadAll → ks.Unwrap → envelope.Decode →
    codec.Decrypt` (modelled by `storage.OpenBytes`, `blob.go:171-205`).
  - `Backend.Has` (pin present), `Backend.BlockstoreHas` + `Backend.BlockGet`
    (block fixity), `Backend.Get` (`internal/ipfs/backend.go:72-98`).
  - Deterministic-import constants (`HashAlg`, `CodecRaw`/`CodecDagPB`,
    `RawCodecThresholdBytes`) in `internal/ipfs/importrules.go`.
  - `GetDEKByBlob` returns `wrapped_key`, `state`, `master_key_version_id`
    (`internal/db/queries/blobs.sql:14`).
- **Wiring pattern.** Subsystems are built in `coordinator.New` gated on
  `pool`/`backend`/`ks` (signed-URL stack at `coordinator.go:207`), and started as
  goroutines in `Run` (`revocations.RefreshEvery`, `gcLoop`, `workers.Run` —
  `coordinator.go:399-409`). `gcLoop` (`:440`) is the precedent for periodic DB
  housekeeping. The admin route group (`internal/api/server.go:128`,
  `RequireRole("operator","moderator")`) mounts present handlers and falls through to
  `adminNotFound`; `ServerConfig` carries nil-able handler pointers (M7 `SigningAdmin`,
  `server.go:51`).
- **`cmd/coordinator/main.go`** is env-driven; it builds `pool`, keystore (+ Bootstrap),
  the signing-key bootstrap, auth, and the embedded backend, then calls
  `coordinator.New(pool, backend, ks, Config{…})` (`main.go:178`) and `c.Run(ctx)`.
- **Test harness.** `internal/dbtest` (Postgres testcontainer), `internal/blobfixture`
  (seed blobs with known plaintext), and the nginx-fronted integration pattern
  (`internal/integration/m7_signed_urls_test.go`) are all available.

## Architecture

```
coordinator.New (pool + backend + ks present)
   ├─ build Scheduler(checks, recorder, cadences, clock, logger)
   ├─ build Maintainer(pool, retention)            // partitions + pruning
   └─ build AuditAdminHandler(queries)              // sc.AuditAdmin

coordinator.Run
   ├─ go scheduler.Run(ctx)        // alongside revocations refresh / gcLoop / workers
   └─ go maintainer.Run(ctx)

scheduler.Run  — single goroutine, ~10 s tick:
   lastRun ← SELECT audit_kind, MAX(audited_at) GROUP BY audit_kind   // seed at boot
   on each tick, for each enabled kind where now-lastRun[kind] ≥ interval[kind]:
        if not already running:  go runKind(kind)   // per-kind no-overlap guard
   runKind: ctx, cancel := context.WithTimeout(ctx, budget[kind]); defer cancel
        sample N rows → per item: pass | fail | skip → recorder.Record(rows)
        lastRun[kind] = now

recorder.Record(rows):
   pgx.Batch INSERT INTO integrity_audits (cid, audit_kind, result, error)
   for each fail: slog.Warn("integrity audit failed", cid, audit_kind, error)
                  sink.AuditFailed(ctx, AuditFailure{cid, kind, error})   // log-only

maintainer.Run — single goroutine, 24 h tick (and once at boot):
   ensure partitions for [this month .. +2 months] exist (CREATE … IF NOT EXISTS)
   DELETE pass rows older than passRetention (30 d)
   DROP partitions whose whole range is older than failRetention (≥ 1 y)

admin: GET /api/v1/admin/audits/integrity?page&per_page&result&audit_kind
       RequireRole(operator,moderator) → PaginatedIntegrityAudits
```

The audit **deliberately bypasses storage visibility/authz.** `storage.Resolve` enforces
the private-content gate and is built for *outbound* serving; an audit must read every
blob regardless of visibility and must never serve bytes outward. So the checks reuse the
low-level decrypt *primitives* (`backend` + `keystore` + `envelope`) directly rather than
calling `Resolve`/`OpenBytes`. This is sound — the audit only reads ciphertext and keys the
coordinator already holds, computes a boolean, and discards the plaintext.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/audit/integrity` | `Scheduler`, the seven `Check`s, `Recorder`, `FailureSink`, `Maintainer`, cadence config | `db/gen`, `envelope` (Decode/Unwrap), `ipfs.Backend`, `go-cid`, `log/slog` |
| `internal/db` (`audit.sql` → `gen`) | sampling + batch insert + list/count + schedule seed | — |
| `internal/api/handlers` | `AuditAdminHandler` (`audits_admin.go`) | `db/gen`, `bearer`, `httputil` |
| `internal/api/httputil` | `pagination.go` — parse `page`/`per_page`, build `Pagination` | — |
| `internal/api` (`server.go`) | mount the admin list route; `ServerConfig.AuditAdmin` | `handlers` |
| `pkg/coordinator` | `Config.IntegrityAudit`; build Scheduler+Maintainer+AuditAdmin; start in `Run` | `audit/integrity`, `ipfs`, `envelope` |
| `cmd/coordinator` | populate cadence/retention defaults onto `Config` | `coordinator` |

`internal/audit/integrity` imports only lower-level packages (`envelope`, `ipfs`, `db/gen`)
— no cycle, and notably **no dependency on `pkg/coordinator/storage`** (it does not go
through the authz-gated read path) and **none on `internal/jobs`** (it does not enqueue
persistent work).

## The seven audit kinds

Each kind implements a small `Check` contract: it samples up to `SampleSize` candidate
rows and returns one `(cid, result, error)` per candidate. `result = skip` records that the
check did not apply to that candidate (the `audit_result` enum carries `skip` for exactly
this).

| `audit_kind` | Sample source | Mechanism | pass / fail / skip |
|---|---|---|---|
| `envelope_decode` | random `active` encrypted blobs | `backend.Get(cid)` → `io.ReadFull` first 32 B → `envelope.Decode` | decodes / `ErrEnvelope*` / unencrypted (`public_archival`) |
| `key_unwrap` | random `active` encrypted blobs' DEKs | `GetDEKByBlob` → `ks.Unwrap(wrapped, mkvID)` | unwraps / unwrap error / DEK `shredded` or unencrypted |
| `sample_decrypt` | random `active` encrypted blobs ≤ size cap | `Get` → `ReadAll` → `Unwrap` → `Decode` → `codec.Decrypt` | tag valid / `ErrEnvelopeAuthFailed` / unencrypted or > cap |
| `kubo_pin_present` | random `active` blobs (originals + derivatives) | `backend.Has(cid)` | pinned / not pinned / — |
| `derivative_state_consistent` | sampled derivatives (`parent_cid IS NOT NULL`) | join parent; compare `state` | states consistent / mismatch / — |
| `block_hash_valid` | random blocks of multi-block blobs (`block_count > 1`) | `backend.BlockGet(block_cid)` → `storedCID.Prefix().Sum(bytes) == storedCID` | match / mismatch or missing block / single-block blob |
| `manifest_consistent` | random blobs with a manifest | `blob_manifests.block_count`/`envelope_size` vs `COUNT(*)`/`SUM(block_size)` over `blob_blocks` | both match / either mismatches / — |

Notes:

- **`integrity_audits.cid` is `NOT NULL`.** `key_unwrap` is conceptually per-key, but it
  records the owning blob's CID — so it samples encrypted *blobs* and unwraps *their* DEK
  ("100 random keys" ≈ 100 random encrypted blobs' DEKs).
- **`envelope_decode` reads only the header.** `Get` returns the stored envelope bytes; the
  check reads exactly `envelope.HeaderSize` (32) bytes via `io.ReadFull` and closes the
  reader, so it never buffers a whole blob.
- **`block_hash_valid` re-derives the CID from the bytes.** `storedCID.Prefix()` carries the
  CID version, codec, and multihash type/length; `Prefix().Sum(blockBytes)` recomputes the
  CID deterministically (the import rules fix `sha2-256` + raw/dag-pb), and a byte-equal
  comparison to the stored CID is the fixity proof. Single-block blobs (`block_count = 1`,
  raw codec, CID == blob CID) are recorded `skip` because they carry no distinct block rows.
- **`derivative_state_consistent`** fails when a derivative's `state` diverges from its
  parent's (e.g. parent `quarantined`, derivative still `active`) — the cascade bug class
  M9's `OnDelete` must avoid. See reconciliation #1 on the sampling vs "past hour" framing.
- **`manifest_consistent`** is pure DB (no Kubo, no keystore): it cross-checks the
  proof-readiness manifest against the recorded block layout.

## Scheduler and cadence

A single goroutine ticks every ~10 s (matching the master design's tick granularity) and
fires any kind whose interval has elapsed. This is simpler than seven independent tickers
and makes "disabled" and "due" uniform to reason about.

- **Per-kind cadence.** `Cadence{Interval time.Duration, SampleSize int}` per kind. Defaults
  from `INTEGRITY_AUDIT.md` § "Schedule": `envelope_decode` 1 h/100, `key_unwrap` 1 h/100,
  `sample_decrypt` 1 h/50, `kubo_pin_present` 15 min/200, `derivative_state_consistent`
  1 h/200, `block_hash_valid` 24 h/100, `manifest_consistent` 24 h/100.
- **Restart resumes mid-cadence.** `lastRun[kind]` is seeded at boot from
  `SELECT audit_kind, MAX(audited_at) … GROUP BY audit_kind`, so a kind that ran 5 min before
  a restart is not re-run for ~55 min. This is how M8 honours "resume from the schedule's
  natural cadence" without persisting any in-flight queue, and avoids a boot-time thundering
  herd of all seven kinds.
- **Bounded, time-boxed runs.** Each `runKind` holds a per-kind "in flight" guard so a slow
  kind never overlaps itself, and runs under `context.WithTimeout` (a per-kind budget) so a
  hung Kubo call or a pathological sample can never pin memory or starve the process. This is
  the isolation the in-process model trades for not having the job system's leasing.
- **Disable + production floor.** `Interval <= 0` disables a kind (dev convenience).
  `EnforceAuditPolicy` refuses a zero interval in production builds (mirrors
  `auth.EnforceAnonymousPolicy` and the `nova_dev` build-tag pattern). Since cadence is not
  operator-exposed in M8, this floor only guards test/cmd overrides.
- **Deterministic entry points.** `RunOnce(ctx)` runs every enabled kind once, synchronously;
  `RunKind(ctx, kind)` runs one. The ticker loop calls the same `runKind`. Tests use these to
  avoid waiting on wall-clock cadence; a future `novactl audits run-now` would too.

## Sampling

Sampling uses `ORDER BY random() LIMIT $1` over the candidate predicate (e.g. `state =
'active' AND encryption_key_id IS NOT NULL`). For Phase-1 single-node corpora this is
adequate and exact; it is an `O(n)` scan, so the documented scale path (a future
optimization, not M8) is `TABLESAMPLE SYSTEM` for the large `blobs` / `blob_blocks` tables.
All sampling, the batched insert, and the listing/count live in
`internal/db/queries/audit.sql` (sqlc-generated). Sample sizes are clamped to the spec's
sane bounds (`1..10000`).

## Retention and partition lifecycle

`integrity_audits` is RANGE-partitioned by month, and the committed partitions stop at
**2026-07-01**. A partitioned table with no covering partition **rejects** out-of-range
inserts, so without create-ahead the scheduler's inserts begin failing on 2026-07-01 — about
a month out. The `Maintainer` therefore runs at boot and on a 24 h tick and:

1. **Creates ahead.** Ensures partitions exist for the current month plus the next two
   (`CREATE TABLE IF NOT EXISTS integrity_audits_YYYY_MM PARTITION OF integrity_audits FOR
   VALUES FROM ('YYYY-MM-01') TO ('next-month-01')`). Idempotent.
2. **Prunes passes.** `DELETE FROM integrity_audits WHERE result = 'pass' AND audited_at <
   now() - passRetention` (default 30 d). Failures are kept.
3. **Drops aged partitions.** `DROP TABLE integrity_audits_YYYY_MM` once a whole month's range
   is older than `failRetention` (default ≥ 1 y) — reclaiming both the long-retained failures
   and any residual passes in one cheap metadata operation.

Partition names are derived from month boundaries (never from user input), so the DDL is
assembled with `fmt.Sprintf` and run via `pool.Exec` — DDL with dynamic identifiers is outside
sqlc's parameterized-query model. The mechanism generalizes to `audit_log` (also monthly-
partitioned), but M8 keeps the `Maintainer` scoped to `integrity_audits`; M9 can extract a
shared helper when it wires `audit_log`.

## Failure surfacing

"Failure surfacing" (the roadmap's M8 phrase) is three concrete channels, no metric:

1. **Structured logs.** `slog.Warn("integrity audit failed", "cid", …, "audit_kind", …,
   "error", …)`. Never key material, plaintext, or envelope bytes.
2. **Durable rows.** Every run inserts one row per sampled candidate; failures are
   greppable via `result <> 'pass'` (served by `integrity_audits_failures_idx`).
3. **Admin listing.** `GET /api/v1/admin/audits/integrity` (below) — the surface M11's
   "recent failures" panel consumes.

The `Recorder` also calls a `FailureSink` per failure:

```go
type AuditFailure struct { CID, Kind, Detail string }
type FailureSink interface { AuditFailed(ctx context.Context, f AuditFailure) }
```

M8 ships a log-only default. This is the seam for the deferred `integrity.audit_failed`
webhook (and any future metric counter): the delivery implementation drops in behind the
interface without changing the audit core.

## HTTP contract

### Route

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/api/v1/admin/audits/integrity` | `RequireRole("operator","moderator")` | `?page&per_page&result&audit_kind` → `200 PaginatedIntegrityAudits` |

Mounted in the `server.go` admin group beside the M7 signing routes. `ServerConfig.AuditAdmin`
is nil-able; when nil (a no-pool test coordinator) the path falls through to `adminNotFound`,
exactly like `SigningAdmin`.

### Pagination (new shared helper)

This is the repo's first paginated endpoint. `internal/api/httputil/pagination.go` parses
`page` (default 1, min 1) and `per_page` (default 50, capped at 100), maps to `LIMIT`/`OFFSET`,
and builds the `Pagination{page, per_page, total}` block. `total` comes from a `Count` query
under the same filters. Filters: `result ∈ {pass,fail,skip}` and `audit_kind ∈` the seven
kinds; both optional; invalid values → `400 invalid_request`. Default ordering is
`audited_at DESC`.

### Error → status

| Condition | Status | `code` |
|---|---|---|
| no bearer token | 401 | `unauthenticated` |
| authenticated but not operator/moderator | 403 | `forbidden` |
| bad `result` / `audit_kind` / pagination param | 400 | `invalid_request` |
| ok | 200 | — |

## Configuration

```go
// pkg/coordinator/coordinator.go
type IntegrityAuditConfig struct {
    Enabled        bool                      // master switch (default true)
    Cadences       map[integrity.Kind]integrity.Cadence // per-kind interval+sample; zero ⇒ DefaultCadences()
    PassRetention  time.Duration             // default 30 d
    FailRetention  time.Duration             // default 365 d
}
```

`coordinator.New` builds the Scheduler + Maintainer + `AuditAdminHandler` only when
`pool + backend + ks` are all present (matching the M7 signed-URL gate). Zero-valued cadences
fall back to `integrity.DefaultCadences()`. `cmd/coordinator/main.go` populates the defaults
and documents an optional `NOVA_INTEGRITY_AUDIT_ENABLED` global toggle; no per-kind env knobs
(the 14-knob alternative was rejected, and `operator.yaml` decode stays deferred).

## Startup and wiring

- **`coordinator.New`** (when `pool+backend+ks`): construct the seven checks over `gen.New(pool)`
  + `backend` + `ks`; construct the `Recorder` (log-only `FailureSink`); construct the
  `Scheduler` (seeded lazily on first `Run`) and the `Maintainer`; set `sc.AuditAdmin =
  handlers.NewAuditAdminHandler(gen.New(pool))`.
- **`coordinator.Run`**: `go scheduler.Run(ctx)` and `go maintainer.Run(ctx)`, beside the
  existing `revocations.RefreshEvery` / `gcLoop` / `workers.Run` starts.
- **No new refuse-to-start** beyond `EnforceAuditPolicy` on an out-of-range zero interval.
  A scheduler with `Enabled=false` simply never ticks.

## Security and privacy considerations

- **Reads what it owns, serves nothing.** The audit bypasses the visibility gate by design;
  it never returns blob bytes on any wire. It only computes booleans over ciphertext + keys
  the coordinator already holds.
- **No secret leakage in logs.** Failure logs carry `cid`, `audit_kind`, and an error string —
  never the master key, a DEK, plaintext, or envelope bytes. (`key_unwrap` failures log the
  error class, not the wrapped bytes.)
- **Transient plaintext.** `sample_decrypt` holds decrypted plaintext in memory only long
  enough to confirm the AEAD tag, bounded by a size cap and the small sample; only the boolean
  result is retained.
- **Bounded, isolated execution.** Per-kind no-overlap guards + per-run `context` timeouts cap
  memory and CPU; the loop is fully decoupled from the write-heavy `jobs.Queue`, so audit I/O
  cannot starve user-facing async work (image prewarm, etc.).
- **Paranoid mode unaffected.** No new IP retention; audit rows key on `cid`, not on any
  viewer or source identity.

## Testing strategy

### Unit (`internal/audit/integrity`)

- **Each check, pass + fail + skip** (table-driven, `dbtest` + `blobfixture` + an offline
  embedded backend or a small fake `Backend`): a clean encrypted blob passes `envelope_decode`
  / `key_unwrap` / `sample_decrypt`; a tampered envelope fails `sample_decrypt`
  (`ErrEnvelopeAuthFailed`) and a corrupted header fails `envelope_decode`; an unpinned CID
  fails `kubo_pin_present`; a parent/derivative state mismatch fails
  `derivative_state_consistent`; a block fetched and re-hashed matches (`block_hash_valid`
  pass) while a single-block blob is `skip`; a manifest with a wrong `block_count`/`envelope_size`
  fails `manifest_consistent`; `public_archival` (unencrypted) blobs are `skip` for the
  crypto checks.
- **Scheduler**: due-ness honoured (seeded `lastRun`); a disabled kind never runs; a kind does
  not overlap itself; a check exceeding its budget is cancelled by the timeout; `RunOnce`
  drives every enabled kind.
- **Recorder**: a batch of mixed results inserts one row each; failures hit the `FailureSink`
  and the warn log.
- **Maintainer**: creates the next two monthly partitions; prunes `pass` rows older than the
  window while keeping failures; drops a synthetic aged partition.
- **Pagination helper + handler**: param parsing/caps; filter validation; `total` correctness;
  role guard.

### Integration (`internal/integration/m8_integrity_audit_test.go`, nginx-fronted, testcontainers)

End-to-end against the untagged coordinator behind nginx (Postgres + nginx containers), in the
M7 test's shape:

1. Boot with fast cadences; create an `operator`; upload an encrypted blob, a `public_archival`
   blob, a multi-block (> 1 MiB) blob, and an image derivative.
2. `RunOnce` → `GET /api/v1/admin/audits/integrity` shows `pass` rows for the applicable kinds
   and `skip` rows where expected (single-block `block_hash_valid`, unencrypted crypto checks).
3. **The spec's canonical failure test:** `backend.Unpin` a known CID, `RunKind(kubo_pin_present)`,
   then `GET …?result=fail&audit_kind=kubo_pin_present` returns that CID's failure row.
4. Pagination (`page`/`per_page`) and filters behave; ordering is `audited_at DESC`.
5. Authz: operator and moderator get `200`; no token `401`; a non-admin role `403`.
6. The `Maintainer` created the partition covering "next month" (assert the partition exists).

### CI

- Build + unit-test the untagged binary (the production path).
- Integration is `-short`-skippable like M2–M7; full run in the integration job.
- `make codegen-check` after regenerating from `audit.sql`.
- Heed the gofmt toolchain skew (do not reformat pre-existing files); `golangci-lint` runs in CI.

## File structure

### Created in M8

```
internal/audit/integrity/config.go            Kind enum, Cadence, DefaultCadences, EnforceAuditPolicy
internal/audit/integrity/config_test.go
internal/audit/integrity/scheduler.go          Scheduler: tick, due-ness seed, bounded runs, RunOnce/RunKind
internal/audit/integrity/scheduler_test.go
internal/audit/integrity/checks.go             the seven Check implementations
internal/audit/integrity/checks_test.go
internal/audit/integrity/recorder.go           batch insert + warn log + FailureSink dispatch
internal/audit/integrity/sink.go               FailureSink interface + log-only default
internal/audit/integrity/retention.go          Maintainer: partition create-ahead + prune/drop
internal/audit/integrity/retention_test.go
internal/db/queries/audit.sql                  sampling + batch insert + list + count + schedule seed
internal/api/handlers/audits_admin.go          listIntegrityAudits handler
internal/api/handlers/audits_admin_test.go
internal/api/httputil/pagination.go            page/per_page parse + Pagination response
internal/api/httputil/pagination_test.go
internal/integration/m8_integrity_audit_test.go  end-to-end through nginx
```

### Modified in M8

```
internal/db/gen/*                              regenerated from audit.sql
internal/api/server.go                         mount GET /admin/audits/integrity; ServerConfig.AuditAdmin
pkg/coordinator/coordinator.go                 Config.IntegrityAudit; build Scheduler+Maintainer+AuditAdmin; start in Run
cmd/coordinator/main.go                        populate IntegrityAudit defaults; NOVA_INTEGRITY_AUDIT_ENABLED
internal/config/types.go                       comment only (structs already match)
docs/specs/INTEGRITY_AUDIT.md                  reconciliations #1 (a)–(e)
docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md  in-process model reconciliation (#2)
docs/specs/openapi.yaml                        confirm/complete listIntegrityAudits + page params (#3)
docs/ROADMAP.md                                M8 status + links + tag (#4)
```

### Reused unchanged

```
internal/envelope/envelope.go                  Decode (envelope_decode)
internal/envelope/keystore.go                  Unwrap (key_unwrap, sample_decrypt)
internal/ipfs/backend.go                       Has, BlockGet, BlockstoreHas, Get
internal/ipfs/importrules.go                   HashAlg / codec / threshold constants
pkg/coordinator/storage/blob.go                OpenBytes decrypt pipeline (reference)
internal/api/httputil/                          WriteError (Error shape)
internal/auth/bearer/                           RequireRole guards
internal/dbtest, internal/blobfixture           test harness
```

## Risks and notes

- **`ORDER BY random()` is `O(n)`.** Acceptable for Phase-1 single-node corpora; `TABLESAMPLE
  SYSTEM` is the documented scale path if a deployment's `blobs`/`blob_blocks` grow large.
- **`sample_decrypt` decrypts whole v1 envelopes.** The spec's "small random byte range" is a
  Phase-2 streaming-AEAD framing; v1's single-shot tag covers the whole envelope. A size cap
  plus the small sample (50) bound the cost; oversized blobs record `skip`.
- **`derivative_state_consistent` is mostly inert until M9.** No state-change timestamp exists
  in Phase 1, so M8 samples derivatives and compares to the parent's current state. Until M9
  adds quarantine/tombstone cascades there is nothing to fail; the check is in place precisely
  to police those cascades when they arrive.
- **Partition create-ahead is non-negotiable.** The committed partitions end at 2026-07-01; the
  `Maintainer` must create July's (and beyond) at boot or inserts begin failing within ~a month.
- **Metric + webhook deferred (decided).** The `FailureSink` seam keeps the audit core stable
  for when they land; `INTEGRITY_AUDIT.md` reconciliation #1 records the deferrals so the spec
  and code do not silently diverge.

## Cross-references

- `docs/specs/INTEGRITY_AUDIT.md` — the normative behaviour M8 implements (seven kinds,
  schedule, reporting, retention).
- `docs/specs/DATA_MODEL.sql` — `integrity_audits`, `audit_kind`, `audit_result`;
  `internal/db/migrations/0003_partitions.sql` — the monthly partitioning.
- `docs/specs/openapi.yaml:1252,2037,2101` — `listIntegrityAudits`, `IntegrityAudit`,
  `PaginatedIntegrityAudits`.
- `docs/specs/ENCRYPTION_ENVELOPE.md` — envelope format + key-unwrap semantics the crypto
  checks verify; `docs/specs/IPFS_IMPORT_RULES.md` — the deterministic CID rules
  `block_hash_valid` re-derives against.
- `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` § "Integrity audit
  loop" — reconciled to the in-process model (reconciliation #2).
- `docs/superpowers/plans/2026-06-02-phase1-m8-integrity-audit-scheduler.md` — the
  implementation plan.
- M7 design (`2026-06-01-phase1-m7-signed-urls-design.md`) — the admin-route + `ServerConfig`
  + background-loop patterns M8 reuses.
- `docs/specs/POSSESSION_AUDIT.md` — the Phase-2 donor-facing audit this local audit
  deliberately is *not*.
