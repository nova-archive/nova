# Phase 1 M10 — Master-Key Rotation Design

## Purpose and scope

M10 is the tenth Phase-1 milestone and the fifth (final) of the backend-capability band (M6–M10). It is
Nova's **master-key rotation backbone**: the milestone that finally *drives* the key-versioning machinery
every prior milestone quietly built. Until now the `master_key_versions` table has had a `state` it could
only ever set to `active`, the `key_state` enum's `rotating` value has had no writer, the
`dek_master_version_idx` partial index (`(master_key_version_id) WHERE state IN ('active','rotating')`) has
indexed rows nothing ever scanned, and the keystore's multi-version loading
(`NewKeystoreFromEnv` resolves every `NOVA_MASTER_KEY_<LABEL>`) has had no rotation to feed. M10 wires all of
it into a coherent operator workflow: introduce a new master-key version and re-wrap every secret wrapped
under the old version — online, in parallel, with no read-path downtime — then retire the old version so the
operator can drop its key material.

The behaviour is already largely **normative**: `docs/specs/ENCRYPTION_ENVELOPE.md` § "Master key versioning"
and § "Rotation procedure (Phase 1 deliverable)" specify the algorithm step-by-step, and the roadmap
(`docs/ROADMAP.md` M10 row) commits `novactl keys rotate-master`, `/api/v1/admin/keys/rotate-master`, a
**parallel re-wrap worker**, and **reads working against either MK version during rotation**. M10 implements
those; drift between them and the code is a bug in the code — with two deliberate, documented reconciliations
(the per-row crash-safety model and a vestigial CLI flag; see "Source of truth").

The package path `internal/masterkey` is absent today; M10 creates it. **There is no new migration** — every
table M10 touches (`master_key_versions`, `data_encryption_keys`, `signing_keys`) ships in `0001_init.sql`,
and the partial index the worker scans is already present.

This is the **last forward dependency M7 left open**: M7 introduced `signing_keys.wrapped_key`, the first
non-DEK table wrapped under the operator master key, and flagged in three co-located places that M10's re-wrap
MUST also walk `signing_keys` (state `active` + within-grace `retired`) or it orphans every signing key on
rotation and breaks signed-URL verification. M10 closes that dependency.

### In scope

- **`internal/masterkey`** — a `Rotator` with `Start(ctx, from, to)` (validate → mark the source version
  `rotating` → spawn the worker pool, non-blocking), `Status(ctx)` (progress projection), and
  `ResumeIfRotating(ctx)` (boot-time crash recovery). A **bounded parallel worker pool** (configurable
  concurrency) that claims batches of DEK ids (`FOR UPDATE SKIP LOCKED`), re-wraps each row via one atomic,
  version-guarded `UPDATE`, then re-wraps the signing keys, then retires the source version. Domain errors
  (`ErrToNotActive`, `ErrFromNotLoaded`, `ErrAlreadyRotating`, `ErrUnknownVersion`).
- **Keystore accessors** (`internal/envelope/keystore.go`) — read-only `HasLabel`, `LoadedLabels`,
  `VersionID(label)`, `ActiveVersionID()` over the existing in-memory maps, so the endpoint can validate that
  both versions are loaded before starting and the worker can resolve label→id for its guard. No new wrapping
  path: `Wrap()` already targets the active label.
- **`internal/db/queries/masterkey.sql`** — the sqlc surface: DEK claim (SKIP LOCKED) + guarded re-wrap +
  count; signing-key list + guarded re-wrap + count; master-version get/list(+counts)/state-transition.
- **`/api/v1/admin/keys/rotate-master`** (operator-only) — validate, mark `rotating`, kick the worker, return
  `202` with the totals; **`GET /api/v1/admin/keys/rotation-status`** (operator) — the live progress + a
  `stalled` indicator.
- **`novactl keys`** — a new subcommand group: `rotate-master --from --to [--no-confirm]` (triggers, then
  polls to completion with a progress line) and `status` (version table + drain progress). A thin HTTP client
  mirroring `novactl signed-url sign`.
- **Stalled-rotation observability** — a `master_key_rotation` `/readyz` check (degraded when a `rotating`
  version's key is not loaded), a `stalled` field on `rotation-status`, and a persistent WARN log, so a
  prematurely-dropped source key surfaces in the operator's monitoring instead of as silent decrypt failures.
- **WAL/bloat pacing** — a configurable inter-batch pace so a million-row re-wrap does not saturate disk I/O
  or outrun autovacuum; operator autovacuum guidance for `data_encryption_keys` in the runbook.
- **Transient-plaintext zeroing** — the worker `defer`-zeroes the unwrapped per-blob/signing key buffer after
  re-wrap (best-effort heap hygiene; see Security).
- **Audit** — best-effort `master_key.rotation_started` / `…_resumed` / `…_completed` entries via the M9
  `auditlog.Writer`.
- Unit tests + an nginx-fronted integration test (two-boot, mirroring the real operator workflow) proving the
  four exit criteria.

### Out of scope (with the milestone/owner that holds each)

- **Runtime key loading / no-restart activation** — **not planned.** The keystore loads material only at boot
  (`NewKeystoreFromEnv`); a restart is required to introduce the new key regardless, and runtime secret
  injection is a strictly worse posture (`THREAT_MODEL.md` boundary ③). The restart that loads v2 also flips
  `NOVA_MASTER_KEY_ACTIVE=v2`, which is why rotation is a converging *drain* and not a moving target.
- **Master-key *generation* helper** (`novactl keys gen-master`) — **a later convenience.** The operator
  mints a version with `openssl rand -hex 32` (the README dev recipe and `OPERATOR_CHECKLIST.md`); M10 does
  not add a generator.
- **Cross-node rotation propagation** — **Phase 2.** Phase 1 is single-node; the rotation is local. The mesh
  fan-out lands with federation.
- **`novactl keys rotate-signing` wrapper** — **deferred (optional).** M7 left signing-key rotation as an
  endpoint with no CLI; M10 introduces the `keys` group but keeps it focused on the master key. Wrapping
  `rotate-signing` is a one-handler consistency win for a later pass.
- **Scheduled / automatic master-key rotation** — **not planned.** Rotation is a deliberate, audited operator
  action, not a cadence-driven background task (unlike the M8 scheduler / M9 sweep).
- **HSM / KMS master-key backends** — **explicit non-goal** (`ENCRYPTION_ENVELOPE.md` § "What this spec
  deliberately does not specify"). Operators with such needs wrap `NOVA_MASTER_KEY` loading to fetch from
  their KMS at boot.
- **`operator.yaml` decode for the M10 knobs** — still deferred (M5–M9 precedent); env knobs only.
- **A general Kubo-pinset/DB reconciliation** — Phase-5 hardening (unrelated to key wrapping).

## Source of truth and required doc reconciliations

1. **`docs/specs/ENCRYPTION_ENVELOPE.md` § "Rotation procedure (Phase 1 deliverable)" — reconcile to the
   implemented behaviour.** The Phase-0 sketch has three points of drift from the safest implementation:
   (a) **Per-row state.** The sketch marks each DEK `state='rotating'` (step 3a) then back to `'active'`
   (step 3d). M10 instead performs **one atomic, version-guarded `UPDATE`** that flips `wrapped_key` and
   `master_key_version_id` together (state stays `active`). This is strictly safer for crash recovery: a
   per-row `rotating` mark creates an ambiguous state on crash (did the row already re-wrap?), whereas the
   atomic flip leaves every row in exactly one of two consistent states (old wrapped+old id, or new
   wrapped+new id). The `rotating` state therefore marks the **version row**, not each DEK; the partial index
   keeps `active` (and `rotating`, harmlessly) in its predicate.
   (b) **CLI flag.** Drop the vestigial `--new-key-env NOVA_MASTER_KEY_V2`: the coordinator already loads
   every `NOVA_MASTER_KEY_<LABEL>` at boot, so the CLI passes only `--from`/`--to` (labels), never key
   material over the wire.
   (c) **Activation precondition + invariant.** Document that the operator sets `NOVA_MASTER_KEY_ACTIVE=<to>`
   and restarts *before* triggering rotation, and that `rotate-master` enforces `to == active label`.
   Also flip the "(Phase 1 deliverable)" / "(realised in M10)" notes to "implemented in M10
   (`internal/masterkey`)".
2. **`docs/specs/DATA_MODEL.sql` — `master_key_versions` + DEK comments.** Note that `rotating` marks the
   in-progress *source* version; `dek_master_version_idx` is the re-wrap worker's scan index; a version row is
   never deleted (a `shredded` DEK keeps its FK to it forever) — only `retired`; and a re-wrap is an atomic
   guarded `UPDATE`, not a shred (legal-hold rows are re-wrapped, not skipped). Schema objects stay
   migration-only (M6 reconciliation precedent); this is comment-level.
3. **`docs/specs/openapi.yaml` — add the two paths.** `POST /api/v1/admin/keys/rotate-master`
   (`RotateMasterRequest{from_version,to_version}` → `202 RotationStartedResponse`; `409 rotation_in_progress`;
   `400 invalid_request`) and `GET /api/v1/admin/keys/rotation-status` (`200 RotationStatusResponse`).
   Document the `401 unauthenticated` / `403 forbidden` (operator-only) responses.
4. **`docs/legal/OPERATOR_CHECKLIST.md` — the rotation runbook + the backup mandate.** Add the five-step
   rotation procedure, the "back up **every** MK version out-of-band; loss of all versions = permanent loss of
   every blob" warning (`ENCRYPTION_ENVELOPE.md` § Constraints mandates documenting this here), the
   "do not drop the old key until `rotation-status` shows it `retired` with 0 referencing rows" caution (the
   stalled-rotation failure mode), and autovacuum guidance for `data_encryption_keys` on large deployments.
5. **`docs/ROADMAP.md` + the master plan — the M10 row.** Mark status, link this design + its implementation
   plan, record the `m10-master-key-rotation` tag on completion, and record the deferrals (runtime activation
   → not planned, generator → later, cross-node → Phase 2, `rotate-signing` wrapper → optional).
6. **`docs/THREAT_MODEL.md` boundary ③ — a one-line note** that both MK versions are process-resident during
   a rotation window (already implied by multi-version loading; made explicit).

## Preconditions from M1–M9 (confirmed in committed code)

- **Schema present, no new tables** (`internal/db/migrations/0001_init.sql`):
  - `master_key_versions (id uuid PK, version_label text UNIQUE, state key_state DEFAULT 'active',
    created_at, retired_at)` (`:175`); `master_key_versions_state_idx` (`:183`).
  - `data_encryption_keys (id, algorithm, wrapped_key bytea, master_key_version_id uuid NOT NULL FK,
    legal_hold, state key_state DEFAULT 'active', created_at, shredded_at)` (`:205`) with
    **`dek_master_version_idx ON (master_key_version_id) WHERE state IN ('active','rotating')`** (`:221`) —
    *the re-wrap worker's claim index, designed ahead for M10* — and `no_shred_under_legal_hold CHECK`
    (`:215`).
  - `signing_keys (kid PK, algorithm, wrapped_key, master_key_version_id NOT NULL FK, state, active_from,
    retire_after, created_at)` (`:233`); `signing_keys_state_idx`.
  - `key_state` enum = `active | rotating | shredded | retired` (`:130`).
- **Keystore present** (`internal/envelope/keystore.go`):
  - `NewKeystoreFromEnv(pool)` (`:63`) loads **every** `NOVA_MASTER_KEY_<LABEL>` through the M6.1 resolver
    chain (`env → _FILE → /run/secrets/master-key-<label>`), with the `ACTIVE`/`FILE` pseudo-labels filtered;
    a declared label that resolves from no source is **fatal at startup** — so all loaded versions are
    genuinely present. `activeLabel` comes from `NOVA_MASTER_KEY_ACTIVE` and is **immutable** for the process.
  - `Bootstrap(ctx)` (`:150`) inserts the active label's `master_key_versions` row (idempotent) and caches
    `idByLabel`/`versionByID`. `Wrap(key)` (`:196`) wraps under the active label → `(72-byte wrapped, active
    id)`. `Unwrap(ctx, wrapped, versionID)` (`:221`) resolves the version by id (DB reload on cache miss) and
    unwraps with the matching in-memory key — **this is what makes "reads work against either version" free**,
    given both keys are loaded. `ActiveLabel()` (`:141`) is exposed; `HasLabel`/`LoadedLabels`/`VersionID`/
    `ActiveVersionID` are the M10 additions.
  - `internal/envelope/keywrap.go`: `WrapKey`/`UnwrapKey` (XChaCha20-Poly1305, empty AAD, fresh nonce; 72-byte
    payload). `UnwrapKey` returns the 32-byte plaintext the worker zeroes after use.
- **Rotation precedent (M7).** `SigningAdminHandler.RotateSigning` (`internal/api/handlers/signing_admin.go:62`)
  is the template: operator-only, one `pgx.Tx`, mint/transition, then `keySource.Invalidate()`, structured
  log, best-effort `audit.Write`. Master-key rotation is the larger sibling (a backfill, not a single insert).
- **In-process workers (M8/M9).** The M8 `Maintainer`/`Scheduler` and the M9 `Sweeper` are goroutines started
  in `coordinator.Run` (`pkg/coordinator/coordinator.go:478`), gated on deps, bounded per tick, resumable
  from DB state. The M10 `Rotator.ResumeIfRotating` joins them (`coordinator.go:497–506` is the wiring site).
- **`/readyz` mechanism (M6.2/M7).** `coordinator.New` registers `handlers.ReadyCheck{Name, Fn}` for
  `database`, `signing_keys`, `ipfs`, verifiers (`coordinator.go:312–362`); the handler runs them in parallel
  under a 1 s deadline. The `signing_keys` check (fails when 0 active keys) is the precedent for the M10
  `master_key_rotation` degraded check.
- **Audit writer (M9).** `auditlog.NewWriter(gen.New(pool), slog)` (`internal/auditlog/writer.go`) with
  best-effort `Write`; built in `coordinator.New` and shared (`coordinator.go:234`).
- **CLI (M6/M7/M9).** `cmd/novactl/main.go` is a hand-rolled `flag` dispatcher with `auth`/`signed-url`/
  `moderation` groups, a `~/.config/nova/credentials.json` bearer cache, and `postJSON`/`getJSON` helpers.
  There is **no `keys` group yet** (rotate-signing is endpoint-only) — M10 introduces it.
- **Coordinator/cmd wiring.** `coordinator.New(pool, backend, ks, cfg)` builds subsystems gated on
  `pool`/`backend`/`ks`; `cmd/coordinator/main.go` is env-driven, builds the keystore
  (`envelope.NewKeystoreFromEnv` → `ks.Bootstrap`), and maps `NOVA_*` knobs onto `coordinator.Config`.
- **Test harness.** `internal/dbtest` (Postgres testcontainer), `internal/blobfixture` (seed blobs with known
  plaintext/visibility), the nginx-fronted integration pattern (`internal/integration/m7_…`, `m8_…`,
  `m9_…_test.go`).

## Architecture

```
coordinator.New (pool + ks present)
   ├─ masterkey.NewRotator(gen.New(pool), pool, ks, auditW, slog, RewrapConcurrency, RewrapBatchSize, Pace)
   ├─ handlers.NewMasterKeyAdmin(rotator)              // rotate-master + rotation-status
   ├─ sc.MasterKeyAdmin = …                            // nil-able ServerConfig pointer (operator-only)
   └─ ReadyCheck{ "master_key_rotation", rotator.Readyz }   // degraded on stalled rotation

coordinator.Run
   └─ go rotator.ResumeIfRotating(ctx)     // beside revocations.RefreshEvery / gcLoop / workers /
                                           // audit Maintainer+Scheduler / modSweeper

POST /api/v1/admin/keys/rotate-master  (operator):
   validate: to == ks.ActiveLabel() && ks.HasLabel(to); from loaded, != to, has a version row, not retired;
             no other version already 'rotating'
   → SetMasterVersionState(from, 'rotating')
   → rotator.Start(ctx, from, to)         // non-blocking; spawns the pool against a background ctx
   → 202 {from, to, total_deks, total_signing_keys}

Rotator worker pool (N goroutines):
   loop  ids := ListActiveDEKIDsForVersion(from, batch)  // FOR UPDATE SKIP LOCKED, in a tx
         per id (same tx): row := read; pbk := ks.Unwrap(row.wrapped, fromID); defer zero(pbk)
                           wrapped,_ := ks.Wrap(pbk); RewrapDEK(wrapped, toID, id, fromID)  // guarded
         commit; pace()                                  // configurable inter-batch sleep
   until ListActiveDEKIDsForVersion(from, …) is empty
   then  signing keys: state IN ('active','retired') for from → same unwrap/zero/wrap/guarded-update;
         keySource.Invalidate()
   then  SetMasterVersionState(from, 'retired', retired_at=now); audit master_key.rotation_completed

GET /api/v1/admin/keys/rotation-status (operator):
   { active, in_progress?: {from, remaining_deks, remaining_signing, total_deks, total_signing, started_at,
     stalled}, versions: [{label, state, dek_count, signing_count, retired_at}] }
```

The rotation core **owns the write side of the key-version lifecycle**. It performs its mutations through
low-level sqlc queries (the guarded `UPDATE`, the `master_key_versions` state transitions) and the keystore's
crypto (`Unwrap`/`Wrap`), never through `storage.Put`/`Resolve` — those are outbound-serving paths, whereas a
re-wrap mutates internal key state for every DEK regardless of blob visibility. This mirrors M8's and M9's
decision to bypass the authz-gated read path for a coordinator-internal job.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/masterkey` | `Rotator` (validate, Start, worker pool, Status, ResumeIfRotating, Readyz), domain errors, transient-key zeroing | `db/gen`, `pgxpool`, `envelope` (Keystore), `auditlog`, `go.uuid`, `log/slog` |
| `internal/envelope` (`keystore.go`) | `HasLabel`/`LoadedLabels`/`VersionID`/`ActiveVersionID` accessors | — (read-only over existing maps) |
| `internal/db` (`masterkey.sql` → `gen`) | claim/rewrap/count DEKs + signing keys; master-version get/list(+counts)/state | — |
| `internal/api/handlers` | `MasterKeyAdminHandler` (rotate-master, rotation-status) | `masterkey`, `bearer`, `httputil` |
| `internal/api` (`server.go`) | mount the two admin routes (operator-only); new `ServerConfig` pointer | `handlers` |
| `pkg/coordinator` | build the Rotator + handler; register the `/readyz` check; start `ResumeIfRotating` | `masterkey`, `envelope`, `auditlog` |
| `cmd/coordinator` | rewrap concurrency/batch/pace env knobs | `coordinator` |
| `cmd/novactl` | `keys` subcommand group (HTTP client) | — |

`internal/masterkey` imports `internal/envelope` (the keystore is the crypto authority) and `db/gen`, and is
imported by `internal/api/handlers` and `pkg/coordinator`. No cycles: `envelope` does not import `masterkey`.

## Operator workflow (the activation model)

A restart is **unavoidable** to introduce a new key: the keystore loads material only at boot, and `Wrap()`
uses the immutable boot-time `activeLabel`. Since the restart is required anyway, the same restart sets the
new key active, which makes the worker a pure, **converging** backfill — new uploads already wrap under the
new version, so the old version's backlog only shrinks and the worker terminates deterministically.

```
1. Generate v2:  openssl rand -hex 32 > /run/secrets/master-key-v2   (or NOVA_MASTER_KEY_V2_FILE).
   Keep v1 present (do NOT remove it yet).
2. Set NOVA_MASTER_KEY_ACTIVE=v2; restart the coordinator.
   → keystore loads {v1, v2}; Bootstrap inserts the v2 master_key_versions row 'active' (idempotent).
   → new uploads now wrap DEKs under v2; old blobs still read (v1 loaded).   ← "either version" window
3. novactl keys rotate-master --from v1 --to v2
   → coordinator validates (to == active), marks v1 'rotating', kicks the worker, returns 202.
   → the CLI polls rotation-status, printing progress, until remaining = 0.
4. Worker drains all v1 DEKs + signing keys → v2, then marks v1 'retired' (retired_at = now()).
5. Operator confirms `novactl keys status` shows v1 'retired' with 0 referencing rows, THEN removes v1
   from env/mounts on the next deploy.
```

**The invariant that ties this together: `rotate-master` requires `to == ActiveLabel()`.** Because `ks.Wrap()`
only ever produces the active version, the worker can only re-wrap *to* the active label; requiring `to ==
active` is what enforces that the operator performed step 2's restart-flip. If `to != active`, the endpoint
refuses with: *"set `NOVA_MASTER_KEY_ACTIVE=<to>` and restart before rotating."* The `from` version must be
loaded (so the worker can unwrap), non-active, backed by a `master_key_versions` row, and not already
`retired`.

## The re-wrap worker

### Per-row re-wrap: one atomic, version-guarded UPDATE

```sql
-- RewrapDEK :execrows
UPDATE data_encryption_keys
   SET wrapped_key = $1, master_key_version_id = $2     -- new wrapped bytes, new version id
 WHERE id = $3 AND master_key_version_id = $4;          -- guarded by the OLD version id
```

`wrapped_key` and `master_key_version_id` flip **together**, so a concurrent reader unwrapping that row always
sees a consistent `(wrapped, version)` pair — it never reads new bytes with the old id or vice-versa. The
`WHERE master_key_version_id = $old` guard makes the update **idempotent** (a re-run matches 0 rows) and
**race-safe** (two workers on the same row → the loser updates 0 rows). No per-row `rotating` state is used,
which is what eliminates the crash-recovery ambiguity the Phase-0 sketch's two-step would create. The
`rotating` state lives on the `master_key_versions` row to mark the in-progress version.

### Claiming work

```sql
-- ListActiveDEKIDsForVersion :many   (run inside the batch tx)
SELECT id FROM data_encryption_keys
 WHERE master_key_version_id = $1 AND state IN ('active','rotating')
 ORDER BY id LIMIT $2
 FOR UPDATE SKIP LOCKED;
```

Served by `dek_master_version_idx`. Each batch is one transaction: claim ids (locking them), re-wrap each,
commit. `SKIP LOCKED` gives clean N-worker parallelism with no double-work; the guarded `UPDATE` is the
belt-and-suspenders backstop. The loop ends when a claim returns 0 rows. Because new uploads wrap under the
active (new) version, no new `from` rows appear during the drain — the worker converges.

### Unwrap → wrap, with transient-key zeroing

Per claimed row: `pbk := ks.Unwrap(ctx, row.wrapped_key, fromID)` recovers the 32-byte plaintext per-blob
key; `wrapped, _ := ks.Wrap(pbk)` re-wraps it under the active version; `RewrapDEK(wrapped, toID, id,
fromID)`. The plaintext buffer is **`defer`-zeroed** immediately after the wrap (`for i := range pbk { pbk[i]
= 0 }`), so it does not linger on the Go heap past the per-row scope (see Security for the honest caveat). The
key is never written to disk, logs, or any cache.

### Signing keys (the M7 forward dependency)

After the DEKs drain, the worker re-wraps `signing_keys` with `master_key_version_id = from AND state IN
('active','retired')` — **active and within-grace retired**, both of which still verify signed URLs.
`shredded` signing keys are skipped (their `wrapped_key` is already zeroed). These are few (one active + a
handful of retired), so no `SKIP LOCKED` batching is needed. Each is the same unwrap/zero/wrap/guarded-update.
After re-wrapping, the worker calls the M7 `DBKeySource.Invalidate()` defensively — re-wrap does not change
the secret value, but this clears any cached version association so subsequent unwraps use the fresh row.

### Parallelism, bounding, and WAL/bloat pacing

The pool runs `RewrapConcurrency` goroutines (default 4), each claiming `RewrapBatchSize` ids per tx (default
256). Crucially, in PostgreSQL's MVCC an `UPDATE` is a delete+insert: re-wrapping a million DEKs in a tight
loop generates a million dead tuples and a large WAL spike that, at the tail of the deployment-size
distribution, can saturate disk I/O (starving the read path) and outrun autovacuum (table bloat). To smooth
this, the worker **paces** — a configurable `RewrapPace` sleep between batch commits (default 50 ms;
`0` disables) — and the operator runbook adds autovacuum guidance for `data_encryption_keys`. At batch 256 /
4 workers / 50 ms pace, a 1 M-blob deployment still drains in a few minutes while leaving the database
headroom; small deployments finish near-instantly regardless. (An adaptive backoff on rising transaction
latency is a noted future refinement; the fixed pace is the M10 implementation.)

### Resume on boot (crash recovery)

`ResumeIfRotating(ctx)`, started in `Run`, checks for any `master_key_versions` row in state `rotating`. If
found and **its key is still loaded**, the worker resumes draining (the guarded `UPDATE` makes resumption
idempotent — already-rewrapped rows are simply not re-claimed). If the `from` key is **not** loaded (the
operator dropped it prematurely), the rotation **stalls**: the worker logs a persistent WARN and the
`/readyz` check degrades (see below) rather than spinning on un-unwrappable rows. The rotation completes once
the operator temporarily restores the `from` key and the node is restarted (or the resume retried).

### Rows the worker does *not* touch

- **`public_archival` blobs** — `encryption_key_id IS NULL`; no DEK row exists to re-wrap (the bytes are
  plaintext and the CID is plaintext-addressed).
- **`shredded` DEKs / signing keys** — not in `state IN ('active','rotating')` / `('active','retired')`;
  `wrapped_key` is already zeroed. They keep their old `master_key_version_id` forever — harmless, and the
  reason a `master_key_versions` row is never deleted (the FK), only `retired`.
- **`legal_hold` DEKs** — re-wrapped **normally**. Re-wrap is not a shred; the `no_shred_under_legal_hold`
  CHECK keeps these rows in `active`/`rotating`, so they are in the index and must be re-wrapped. (A doc note
  prevents confusing "held against shred" with "skip on rotate" — a held key whose master version was dropped
  would be just as unreadable.)

## Stalled-rotation observability

A rotation can stall in exactly one way: a `rotating` version whose key the operator removed before the drain
finished. The node keeps serving new uploads (v2) and already-migrated reads (v2), but **un-migrated v1 blobs
silently fail to decrypt** (`Unwrap` → "master key for version v1 not loaded") and the worker sits idle. Pure
logging would surface this only as scattered `500`s. M10 makes it loud through the existing in-tree
observability surface (Prometheus metrics are deferred project-wide since M8):

- **`/readyz` degradation.** A `master_key_rotation` `ReadyCheck` (alongside `signing_keys`) fails when a
  `rotating` version exists whose key is not loaded. This is the correct home — **readiness, not liveness**:
  `/health` is process-alive and an orchestrator may *restart* a node whose liveness probe fails, but a
  restart cannot conjure a missing key, so degrading liveness would cause a restart loop. Readiness degrades
  the node in the operator's monitoring (and orchestrator endpoint health) without restarting it, which is
  exactly "the operator is punished by their monitoring stack until the key is restored." The runbook notes
  this check must not be wired as a *liveness* probe.
- **`rotation-status.stalled = true`** with a human reason, so `novactl keys status` shows it directly.
- **A persistent WARN log** each resume attempt naming the missing version.

(The `ReadyCheck` is scoped to the `rotating`-version case; a stronger invariant — *every* active/rotating DEK
or signing row's version is loaded — is a natural future generalization, noted but not built, to keep the
probe O(small).)

## Keystore additions

Read-only accessors over the existing `masters`/`idByLabel` maps (no behavioural change to `Wrap`/`Unwrap`/
`Bootstrap`):

```go
func (k *Keystore) HasLabel(label string) bool                 // is this version's key loaded?
func (k *Keystore) LoadedLabels() []string                     // all loaded labels (lowercased)
func (k *Keystore) VersionID(label string) (uuid.UUID, bool)   // label → master_key_versions.id (cached)
func (k *Keystore) ActiveVersionID() (uuid.UUID, bool)         // the active version's id
```

The endpoint uses `HasLabel`/`ActiveLabel`/`VersionID` to validate before starting; the worker uses
`VersionID(from)` for the `UPDATE` guard and `Wrap()` (active = `to`) for re-wrapping. No `WrapUnder(label)`
is needed because the activation model guarantees `active == to`.

## DB queries (`internal/db/queries/masterkey.sql`, sqlc)

| Query | Shape | Use |
|---|---|---|
| `ListActiveDEKIDsForVersion` | `:many` (FOR UPDATE SKIP LOCKED) | claim a batch of DEK ids for `from` |
| `GetDEKWrappedKey` | `:one` | read a claimed row's `wrapped_key` |
| `RewrapDEK` | `:execrows` (guarded) | atomic flip of `wrapped_key` + `master_key_version_id` |
| `CountActiveDEKsForVersion` | `:one` | remaining/total DEK progress |
| `ListSigningKeysForVersion` | `:many` | active+retired signing keys for `from` |
| `RewrapSigningKey` | `:execrows` (guarded by kid + old version) | atomic flip for a signing key |
| `CountSigningKeysForVersion` | `:one` | remaining/total signing progress |
| `GetMasterVersionByLabel` | `:one` | resolve label → row (validation) |
| `GetRotatingVersion` | `:one` (nullable) | resume + status + the `409` guard |
| `SetMasterVersionState` | `:exec` | `active`→`rotating`→`retired` (+ `retired_at`) |
| `ListMasterVersionsWithCounts` | `:many` | status: per-version state + DEK/signing counts |

(Several `ListActiveDEKIDsForVersion` + `GetDEKWrappedKey` may collapse into one `SELECT id, wrapped_key …
FOR UPDATE SKIP LOCKED` returning both columns; kept separate above for clarity.) Regenerate
`internal/db/gen/*` via `make sqlc-generate`; `make codegen-check` in CI.

## HTTP contract

### Admin (`/api/v1/admin/*`, operator-only)

| Method | Path | Auth | Result |
|---|---|---|---|
| POST | `/keys/rotate-master` | `RequireRole("operator")` | `202 {from, to, total_deks, total_signing_keys}` |
| GET | `/keys/rotation-status` | `RequireRole("operator")` | `200` status projection (below) |

`rotate-master` is **operator-only**, tightening the `/admin` group's `operator`+`moderator` guard with an
inner `r.With(RequireRole("operator"))` — exactly as M7's `rotate-signing` does. `ServerConfig` gains a
nil-able `MasterKeyAdmin *handlers.MasterKeyAdminHandler` (nil ⇒ the paths fall through to `adminNotFound`,
like `SigningAdmin`/`AuditAdmin`). The handler is built only when `pool + ks` are present.

`rotation-status` body:

```json
{
  "active": "v2",
  "in_progress": {
    "from": "v1", "remaining_deks": 4123, "total_deks": 1000000,
    "remaining_signing_keys": 1, "total_signing_keys": 3,
    "started_at": "2026-06-03T12:00:00Z", "stalled": false
  },
  "versions": [
    {"label": "v2", "state": "active",  "dek_count": 995877, "signing_count": 2, "retired_at": null},
    {"label": "v1", "state": "rotating","dek_count": 4123,   "signing_count": 1, "retired_at": null}
  ]
}
```

`in_progress` is `null` when idle. `stalled` is `true` when the in-progress version's key is not loaded.

### Error → status

| Condition | Status | `code` |
|---|---|---|
| no bearer | 401 | `unauthenticated` |
| authenticated, not operator | 403 | `forbidden` |
| `to_version` != active label, or not loaded | 400 | `to_not_active` |
| `from_version` not loaded / unknown / already retired / == to | 400 | `invalid_from_version` |
| another version already `rotating` | 409 | `rotation_in_progress` |
| malformed body | 400 | `invalid_request` |
| started | 202 | — |
| status | 200 | — |

## CLI: `novactl keys`

A new subcommand group (thin HTTP client; bearer from `~/.config/nova/credentials.json`; `postJSON`/`getJSON`,
mirroring `cmdSignedURLSign`):

```
novactl keys rotate-master --from v1 --to v2 [--no-confirm]   # POST, then poll rotation-status to done
novactl keys status                                           # GET rotation-status; print version table
```

Re-encrypting every key is consequential, so `rotate-master` prompts for confirmation unless `--no-confirm`,
then polls `rotation-status` (e.g. every ~2 s), printing `re-wrapped N/total DEKs, M/total signing keys …`
until `in_progress` clears, and prints the final `v1 retired` line (or a clear `stalled: <reason>` error and
non-zero exit if it stalls). `keys status` prints the version table for the "safe to drop the old key?" check.
`usage()` and the `cmd/novactl` package doc are updated; `main()` gains a `case "keys"`.

## Configuration

```go
// internal/config/types.go
type MasterKeyRotation struct {
    RewrapConcurrency int           `yaml:"rewrap_concurrency"` // worker goroutines; default 4
    RewrapBatchSize   int           `yaml:"rewrap_batch_size"`  // ids claimed per tx; default 256
    RewrapPace        time.Duration `yaml:"rewrap_pace"`        // sleep between batch commits; default 50ms; 0 disables
}
```

On `coordinator.Config.MasterKeyRotation`; `cmd/coordinator/main.go` reads `NOVA_MASTER_KEY_REWRAP_CONCURRENCY`
(default 4), `NOVA_MASTER_KEY_REWRAP_BATCH` (default 256), `NOVA_MASTER_KEY_REWRAP_PACE_MS` (default 50), each
falling back when unset/zero — the M7/M8/M9 env precedent. `operator.yaml` decode stays deferred.

## The audit vocabulary

Best-effort `auditlog.Writer.Write` (the rotation work commits independently per batch, so the audit attaches
around it — the `SigningAdminHandler.SetAuditWriter` precedent):

| Action | When | Payload |
|---|---|---|
| `master_key.rotation_started` | endpoint accepts (**handler**, actor-attributed) | `{from, to}` |
| `master_key.rotation_resumed` | boot-time resume picks up a `rotating` version (**Rotator**, system) | `{from, to}` |
| `master_key.rotation_completed` | source version marked `retired` (**Rotator**, system) | `{from, to, duration_ms}` |

`TargetType = "master_key_version"`, `TargetID = <from label>`. Payloads carry labels and counts, **never key
material**. Actor attribution follows the M7/M9 layering: the **handler** writes `rotation_started` with
`ownerFromContext(ctx)` (it holds the request context), while the background **Rotator** — which runs under
the coordinator's lifecycle context, not a request — writes `rotation_resumed`/`rotation_completed` as
**system actions** (`ActorID = nil`, exactly like the M9 sweep's `Tombstone(actor=nil)`). This keeps
`internal/masterkey` from importing the `handlers` actor helper.

## Security and privacy considerations

- **Transient plaintext zeroing (best-effort, honest).** The worker `defer`-zeroes the unwrapped per-blob /
  signing key buffer after re-wrap, eliminating the lingering heap reference Go's GC would otherwise leave
  until the memory is reused. This is meaningful hardening — under a host compromise or a core dump *after*
  the worker moves on, that buffer is zeroed — but **not a mathematical guarantee**: the AEAD primitive
  (`chacha20poly1305.Open`) allocates its own output buffer internally, and the Go compiler/GC may move or
  copy slices, so transient copies outside the buffer we control cannot be guaranteed absent. We zero what we
  hold. (The M7 `DBKeySource` deliberately *caches* unwrapped signing-key secrets for its TTL; the worker's
  case is different — strictly transient — so zeroing is the right call here.)
- **Both MK versions resident during rotation.** Necessary and already true post-restart; the `THREAT_MODEL.md`
  boundary ③ posture (process-resident master key, host-security responsibility) is unchanged — there are
  simply two keys resident for the rotation window. Documented.
- **Operator-only.** `rotate-master` and `rotation-status` are operator-gated; the worker exposes no new
  external surface. The CLI is a thin client; the endpoint is the trust boundary.
- **Crypto continuity.** Re-wrapping `legal_hold` rows preserves them (no shred; the CHECK remains the floor),
  and re-wrapping `active`+within-grace `retired` signing keys preserves signed-URL verification across the
  rotation (the M7 exit-criterion dependency).
- **No new identity/IP retention.** The audit rows key on version labels and counts; paranoid mode is
  unaffected.

## Exit criteria

1. After `rotate-master v1→v2` completes, **every** active DEK (parents *and* derivatives) references v2, and
   **0** active/rotating DEKs reference v1.
2. Signing keys in state `active` + within-grace `retired` are re-wrapped to v2; signed-URL verify **and**
   mint keep working across the rotation (the M7 forward dependency satisfied).
3. Reads succeed against **both** versions *during* rotation — a not-yet-rewrapped blob decrypts under v1, a
   rewrapped blob under v2 — with no read-path downtime.
4. A rotation interrupted by a coordinator restart **resumes** from DB state (`ResumeIfRotating`) and
   completes; a rotation whose `from` key was prematurely removed **stalls visibly** (`/readyz` degraded,
   `rotation-status.stalled`) rather than failing silently.

## Testing strategy

### Unit (`internal/masterkey`, keystore accessors, handler)

- **Rotator core** (`dbtest` + `blobfixture` + a real keystore with two in-memory versions): seed DEKs under
  v1 (incl. a derivative and a `legal_hold` row), a `shredded` DEK, a `public_archival` (no-DEK) blob, and
  signing keys (active + within-grace retired + shredded). Run a rotation to completion → every active/legal-
  hold/derivative DEK and the active+retired signing keys reference v2 with re-wrappable bytes (assert a fresh
  `Unwrap` under v2 yields the original plaintext); the shredded rows and the no-DEK blob are untouched; v1 is
  `retired`; counts reach 0.
- **Atomicity/idempotency:** two concurrent workers over the same id set re-wrap each row exactly once (no
  lost update, no double-claim); re-running `RewrapDEK` with the new id matches 0 rows.
- **Resume:** pre-seed a `rotating` v1 with a partial drain → `ResumeIfRotating` finishes it. With v1's key
  *not* loaded → it stalls, `Readyz` reports degraded, `Status().stalled == true`, no panic/spin.
- **Validation:** `to != active` → `ErrToNotActive`; `from` unloaded/unknown/retired/==to → the matching
  error; a second `Start` while `rotating` → `ErrAlreadyRotating`.
- **Zeroing:** a unit asserts the worker zeroes its plaintext buffer after use (via a seam/hook that captures
  the buffer post-wrap and checks it is all-zero).
- **Keystore accessors:** `HasLabel`/`LoadedLabels`/`VersionID`/`ActiveVersionID` over a two-version keystore.
- **Handler:** authz (operator → 202/200; moderator → 403; no token → 401); `409` when already rotating;
  `400` codes; the status JSON shape.

### Integration (`internal/integration/m10_master_key_rotation_test.go`, nginx-fronted, testcontainers)

Two boots, mirroring the real operator workflow (Postgres + nginx containers):

1. **Boot #1, `ACTIVE=v1` only.** Create `operator`/`moderator`/`uploader`; upload an encrypted blob (+ an
   image derivative), a `legal_hold` blob (quarantine `--legal-hold` via M9), and a `public_archival` blob;
   mint a signed URL (signing key under v1). Shut down.
2. **Boot #2, `{v1,v2}` loaded, `ACTIVE=v2`.** Sanity: a fresh upload's DEK is under v2; the old blobs still
   read `200` (either-version). `POST /keys/rotate-master {from:v1,to:v2}` as operator → `202`; poll
   `GET /keys/rotation-status` to completion. **Mid-drain** (small batch + pace, or a paced step) assert an
   un-migrated blob still reads `200`. After completion assert **Exit #1–#3**: all DEKs + the signing key on
   v2, the signed URL still verifies (`200`), every blob read `200` throughout, v1 `retired` with 0
   referencing rows; `GET /keys/rotation-status` shows idle; the audit log shows `master_key.rotation_started`
   then `…_completed`.
3. **Authz:** `rotate-master` with a `moderator` token → `403`; no token → `401`.
4. **Crash-resume (Exit #4):** start a rotation, restart boot #2 mid-drain → `ResumeIfRotating` completes it.
   Drop v1 before a restart → `/readyz` degrades and `rotation-status.stalled == true`; restore v1 + restart →
   it completes.

### CI

- `make sqlc-generate && make codegen-check` after `masterkey.sql`.
- `-short`-skippable integration, like M2–M9; full run in the integration job.
- gofmt only the files M10 touches (toolchain-skew rule); `golangci-lint` is CI-only.

## File structure

### Created in M10

```
internal/masterkey/rotator.go                       Rotator: validate, Start, worker pool, Status, ResumeIfRotating, Readyz, zeroing
internal/masterkey/rotator_test.go
internal/db/queries/masterkey.sql                   claim/rewrap/count DEKs + signing keys; master-version get/list/state
internal/api/handlers/masterkey_admin.go            rotate-master + rotation-status handlers
internal/api/handlers/masterkey_admin_test.go
internal/integration/m10_master_key_rotation_test.go  two-boot end-to-end through nginx (the four exit criteria)
docs/superpowers/specs/2026-06-03-phase1-m10-master-key-rotation-design.md   (this file)
docs/superpowers/plans/2026-06-03-phase1-m10-master-key-rotation.md          (the implementation plan)
```

### Modified in M10

```
internal/envelope/keystore.go      HasLabel/LoadedLabels/VersionID/ActiveVersionID accessors
internal/db/gen/*                  regenerated from masterkey.sql
internal/api/server.go             mount /admin/keys/rotate-master + /rotation-status (operator-only); ServerConfig.MasterKeyAdmin
pkg/coordinator/coordinator.go     build Rotator (pool+ks); MasterKeyAdmin handler; master_key_rotation ReadyCheck; ResumeIfRotating in Run; MasterKeyRotation config
cmd/coordinator/main.go            NOVA_MASTER_KEY_REWRAP_{CONCURRENCY,BATCH,PACE_MS} env knobs
cmd/novactl/main.go                keys subcommand group (rotate-master, status) + usage + package doc
internal/config/types.go           MasterKeyRotation section
docs/specs/ENCRYPTION_ENVELOPE.md  reconciliation #1 (atomic guarded update; --from/--to; activation invariant)
docs/specs/DATA_MODEL.sql          reconciliation #2 (rotating = version; index purpose; never-deleted version row)
docs/specs/openapi.yaml            reconciliation #3 (rotate-master + rotation-status paths)
docs/legal/OPERATOR_CHECKLIST.md   reconciliation #4 (rotation runbook + backup mandate + stall caution + autovacuum guidance)
docs/ROADMAP.md                    reconciliation #5 (status + tag + deferrals)
docs/THREAT_MODEL.md               reconciliation #6 (both versions resident during rotation)
```

### Reused unchanged

```
internal/envelope/keywrap.go       WrapKey/UnwrapKey (the crypto under Wrap/Unwrap)
internal/envelope/keystore.go      Wrap/Unwrap/Bootstrap multi-version loading + ActiveLabel
internal/auditlog/writer.go        best-effort Write (M9)
internal/auth/signedurl            DBKeySource.Invalidate (post-rotation cache clear)
internal/auth/bearer               RequireRole guards
internal/api/httputil              WriteError (Error shape)
internal/api/handlers/ready.go     ReadyCheck (the master_key_rotation degraded probe)
internal/dbtest, internal/blobfixture, internal/integration   test harness
```

## Risks and notes

- **Stalled rotation from premature key removal (mitigated).** If the operator drops the `from` key before the
  drain completes, un-migrated blobs become unreadable and the worker idles. Mitigated by the `/readyz`
  degradation + `rotation-status.stalled` + WARN; recovered by temporarily restoring the key. The runbook's
  "don't drop until `retired` with 0 rows" step is the primary prevention. (DB-commit-then-reclaim ordering is
  not relevant here — there is no destructive step until the operator manually removes the key.)
- **MVCC dead-tuple / WAL pressure at scale (mitigated).** A million-row re-wrap generates a million dead
  tuples; the configurable inter-batch pace + autovacuum guidance keep I/O and bloat in check. Adaptive
  latency-based backoff is a noted future refinement.
- **Transient-key zeroing is best-effort (decided).** Go cannot guarantee no transient copies inside the AEAD
  primitive; M10 zeroes the buffer it controls and documents the boundary honestly rather than overclaiming.
- **Re-wrap throughput vs. read latency (bounded).** The pace + bounded concurrency trade a slightly longer
  rotation for read-path headroom — the right call for an online, no-downtime rotation ("resilience over
  speed").
- **`master_key_versions` rows accumulate (accepted).** Each rotation leaves a `retired` row that is never
  deleted (FK from shredded DEKs). The table is tiny (one row per version ever) — no GC needed.

## Cross-references

- `docs/specs/ENCRYPTION_ENVELOPE.md` § "Master key versioning" + "Rotation procedure" + "Crypto-shredding" —
  the normative algorithm M10 implements (reconciled per #1), and the wrap/unwrap semantics.
- `docs/specs/DATA_MODEL.sql` / `internal/db/migrations/0001_init.sql` — `master_key_versions`,
  `data_encryption_keys` (+ `dek_master_version_idx`, the CHECK), `signing_keys`, `key_state`.
- `docs/specs/openapi.yaml` — `/api/v1/admin/keys/rotate-master`, `/api/v1/admin/keys/rotation-status`.
- `docs/legal/OPERATOR_CHECKLIST.md` — the rotation runbook + the out-of-band backup mandate.
- M7 design (`2026-06-01-phase1-m7-signed-urls-design.md`) § "Risks and notes" — the master-key re-wrap
  forward dependency on `signing_keys` (active + within-grace retired) M10 satisfies; `SigningAdminHandler`
  is the operator-only admin-endpoint precedent.
- M8 design (`2026-06-02-phase1-m8-integrity-audit-scheduler-design.md`) / M9 design — the in-process worker,
  resumable-from-DB, dep-gated wiring, and `/readyz` conventions M10 reuses.
- `docs/superpowers/plans/2026-06-03-phase1-m10-master-key-rotation.md` — the implementation plan.
```
