# Phase 1 M9 — Moderation (DMCA + severe-content + blocklist + audit log) Design

## Purpose and scope

M9 is the ninth Phase-1 milestone and the fourth of the backend-capability band (M6–M10). It is Nova's
**moderation backbone**: the milestone that finally *drives* the takedown machinery every prior milestone
quietly built. Until now the `blob_state` enum has had a `quarantined`/`tombstoned` it could never reach,
the `data_encryption_keys.legal_hold` CHECK has guarded a shred that never ran, the M5 `OnDelete` cascade
seam has had no caller, the `audit_log` table has had no writer, and the M8 `derivative_state_consistent`
audit has had no state transitions to police. M9 wires all of it into a coherent operator workflow:

- **DMCA takedown, quarantine-first** — a public `POST /legal/dmca` intake; a quarantine that blocks reads
  and revokes signed URLs while preserving the bytes through the counter-notification window; an in-process
  **scheduled-tombstone sweep** that crypto-shreds and unpins overdue quarantines; counter-notice and
  restore paths for reversibility.
- **Severe-content manual path** — `quarantine --legal-hold` sets `data_encryption_keys.legal_hold = true`,
  and the existing `no_shred_under_legal_hold` CHECK makes crypto-shred **refused at the DB layer**;
  `clear-legal-hold` (operator-only) releases the hold and lets the standard tombstone proceed.
- **Operator-curated CID blocklist** — a deny registry enforced at the read path and the import/commit path.
- **`audit_log` writer** — the durable operator-action trail deferred from M6/M7, plus a paginated
  `GET /api/v1/admin/audit-log`, plus a backfill of the existing M7 admin actions.
- **`/api/v1/admin/moderation/*`** and a **`novactl moderation`** subcommand over it; repeat-infringer
  strike accounting.

The behaviour is already **normative**: `docs/legal/DMCA_PROCEDURE.md` specifies the quarantine and
tombstone transactions step-by-step (the sweep SQL, the counter-notice handling, the repeat-infringer
accounting, the `audit_log` action vocabulary), and `docs/legal/SEVERE_CONTENT_PROCEDURE.md` § "Phase 1
scope (current)" fixes the severe-content surface to exactly the manual `--legal-hold` path. M9 implements
those two runbooks; drift between them and the code is a bug in the code.

The package paths `internal/moderation` and `internal/auditlog` are `.gitkeep`/absent stubs today; M9
populates them. **The only new migration is the `blocklist` table** — every other table this milestone
touches (`moderation_decisions`, `dmca_cases`, `takedown_repeat_infringers`, `audit_log`,
`data_encryption_keys`, `signed_url_revocations`) already ships in `0001_init.sql` / `0003_partitions.sql`.

### In scope

- **`internal/moderation`** — a `Service` with the five transactional operations (`Quarantine`,
  `Tombstone`, `ClearLegalHold`, `Restore`, `CounterNotice`) and a `Sweeper` (the ≈1-minute in-process
  loop that tombstones overdue quarantines). Domain errors (`ErrLegalHold`, `ErrNotQuarantined`, …). It
  takes a `CascadeHook` func seam (wired to `product.OnDelete`) so it does not import the product package.
- **`internal/auditlog`** — a two-mode `Writer`: `WriteTx` (atomic, inside a moderation tx) and best-effort
  `Write` (post-action, error-swallowing) for the M7 backfill; an `Entry` shape over `audit_log`.
- **The CID blocklist** — `0008_moderation.sql` adds `blocklist`; admin CRUD; enforcement at `storage.Resolve`
  (read → 451) and `storage.Put` (import/commit → refused). A **direct indexed PK lookup**, no cache.
- **`internal/db/queries/moderation.sql`** (+ `audit.sql`/`auth.sql` additions) — the sqlc surface
  (decisions, state transitions, DEK shred + legal-hold, DMCA cases, blocklist, repeat-infringer, audit-log,
  the overdue-sweep query).
- **Public `POST /legal/dmca`** — § 512(c)(3) intake into `dmca_cases(status='received')`; rate-limited; no
  action taken.
- **`/api/v1/admin/moderation/*`** — the action endpoints, the moderation queue, DMCA case management, and
  blocklist CRUD; **`GET /api/v1/admin/audit-log`** (paginated). `RequireRole("operator","moderator")`, with
  `clear-legal-hold` the one operator-only action.
- **`novactl moderation`** — `quarantine`, `takedown`, `clear-legal-hold`, `restore`, `list` (a thin HTTP
  client over the admin API, mirroring `novactl signed-url sign`).
- **Repeat-infringer accounting** — strike upsert on each takedown; surfaced in the moderation queue.
- **`audit_log` partition create-ahead** — extend the M8 `Maintainer` so `audit_log` inserts never hit an
  uncovered range (the committed partitions stop at 2026-07-01).
- **M7 audit backfill** — wire the best-effort `Writer` into `signing-key rotate`, `signed-url revoke`,
  `signed-url sign`.
- Unit tests + an nginx-fronted integration test proving the three exit criteria.

### Out of scope (with the milestone that owns each)

- **Perceptual (PDQ/pHash) scan-at-upload and visual re-upload prevention** — **Phase 3.**
  `image_metadata.perceptual_hash` is `NULL` until then (`DATA_MODEL.sql:374`), and envelope CIDs are
  nonce-randomized, so a CID blocklist cannot recognise a re-encrypted copy of the same image. M9 ships the
  operator-curated **CID** registry the Phase-3 matcher will extend; the synchronous hash-scan the openapi
  imagined is reconciled to that reality (reconciliation #1).
- **NCMEC CyberTipline report generation, evidence packaging, and the legal-hold-clear admin SPA** —
  **Phase 4** (`SEVERE_CONTENT_PROCEDURE.md` § "Implementation roadmap"). M9 ships the schema-enforced
  legal-hold gate and the manual `novactl` path only.
- **Federation unpin *broadcast*** — **Phase 2.** Phase 1 is single-node; tombstone does a **local Kubo
  `Unpin`** (`backend.Unpin`). The `pin_assignments` broadcast lands with the mesh.
- **Automatic account suspension at the repeat-infringer threshold** — **deferred.** There is no
  account-state column (the `users` table carries only `role`); M9 *accounts* strikes and surfaces them, and
  suspension stays the documented manual step in `DMCA_PROCEDURE.md` § "Repeat-infringer accounting".
- **`operator.yaml` `takedown_default_action` decode** — still deferred (M5–M8 precedent). M9 makes
  quarantine-first the default and exposes immediate-tombstone via the explicit `takedown` verb/endpoint, so
  no config knob is needed yet.
- **M6 login/refresh auditing** — **intentionally never.** Auditing the hot `/auth/login` path would add
  latency under the login rate-limiter for no operator-action value; routine auth stays on M6's structured
  logs + refresh-family tracking. The "M6/M7 backfill" is scoped to the M7 **admin** actions, which are the
  privileged operator actions worth a durable trail (and the ones M7's design explicitly assigned to M9).
- **`novactl user create`** — a separate concern, not in the M9 roadmap row.
- **A general Kubo-pinset/DB reconciliation sweep** — **Phase-5 hardening** (see Risks: the unpin-after-commit
  orphan window).

## Source of truth and required doc reconciliations

1. **`docs/specs/openapi.yaml` — reconcile the blocklist to CID-based and complete the moderation surface.**
   The committed stubs describe `POST /api/v1/admin/moderation/blocklist` as "Add a **perceptual-hash** entry"
   (`:1091`) and the upload path as a synchronous "hash blocklist scan" (`:556`). Phase 1 has no perceptual
   hash; reconcile both to a **CID** blocklist (operator-curated; the perceptual scan is Phase 3). Confirm /
   complete the schemas and responses for `POST /legal/dmca` (`:1417`),
   `/api/v1/admin/moderation/{quarantine,takedown,restore,counter-notice,clear-legal-hold,queue}`,
   `/api/v1/admin/moderation/blocklist` (+ `DELETE …/{cid}`), `/api/v1/admin/dmca` (+ `/{id}`), and
   `GET /api/v1/admin/audit-log`; document the `401 unauthenticated` / `403 forbidden` admin responses and
   the `451` "rejected by blocklist" upload response (`:589`).
2. **`docs/legal/DMCA_PROCEDURE.md` — pin the Phase-1 realities.** Note the tombstone "federation unpin
   broadcast" is a **local Kubo unpin** in Phase 1 (broadcast = Phase 2); confirm the sweep cadence/SQL match
   the implementation; note the tombstone clears `scheduled_tombstone_at` on the originating quarantine
   decision (so the sweep's partial index stays lean — see Architecture).
3. **`docs/legal/SEVERE_CONTENT_PROCEDURE.md` — confirm the manual path.** The Phase-1 `quarantine
   --legal-hold` and operator-only `clear-legal-hold` match § "Phase 1 scope"; NCMEC/SPA stay Phase 4. The
   `clear-legal-hold` SQL in § "Operator legal-hold-clear procedure" is the implemented behaviour.
4. **`docs/specs/DATA_MODEL.sql` — add the `blocklist` table** (mirrors `0008_moderation.sql`), and note that
   the `audit_log` partition create-ahead now has a runtime owner (the extended Maintainer).
5. **`docs/specs/INTEGRITY_AUDIT.md` — `derivative_state_consistent` is now live.** M8 noted this kind was
   "mostly inert until M9"; with M9's quarantine/tombstone cascades shipping, it now polices a real
   invariant. Tighten the note accordingly.
6. **`docs/ROADMAP.md` + the master plan — the M9 row.** Mark status, link this design + its implementation
   plan, record the `m9-moderation` tag on completion, and record the deferrals (perceptual blocklist →
   Phase 3, NCMEC/SPA → Phase 4, auto-suspension → later, pinset reconciliation → Phase 5).

## Preconditions from M1–M8 (confirmed in committed code)

- **Schema present, no new core tables.** In `internal/db/migrations/0001_init.sql`:
  - `blob_state` = `active | soft_deleted | quarantined | tombstoned` (`:59`); `moderation_rule`
    (`pdq_match | dmca | severe_content | operator_manual | user_report`, `:107`); `moderation_action`
    (`quarantine | tombstone | allow`, `:115`); `dmca_status` (`received | investigating | actioned |
    rejected`, `:121`).
  - `data_encryption_keys (… legal_hold boolean, state key_state, wrapped_key bytea, shredded_at …)` with
    `CONSTRAINT no_shred_under_legal_hold CHECK (legal_hold = false OR state IN ('active','rotating'))`
    (`:205-219`) — **the exit-criterion enforcement, already in the DB.**
  - `moderation_decisions (id, cid, rule, rule_ref, action, decided_by, decided_at, scheduled_tombstone_at,
    legal_hold, notes)` (`:511`) with the partial index `moderation_decisions_scheduled_tombstone_idx
    (scheduled_tombstone_at) WHERE scheduled_tombstone_at IS NOT NULL` (`:526`) — **the sweep's index.**
  - `dmca_cases (id, claimant_name, claimant_email citext, sworn_statement, target_cid, received_at,
    actioned_at, status)` (`:530`); `takedown_repeat_infringers (user_id PK, strikes, last_strike_at)`
    (`:544`).
  - `signed_url_revocations (id, kind CHECK IN ('cid','aud','kid','path_prefix'), value, revoked_at,
    UNIQUE(kind,value))` (`:561`) — **no issuer/source column** (relevant to Restore, below).
  - `audit_log (id bigserial, actor_id, action, target_type, target_id, payload jsonb, at)` is **monthly
    RANGE-partitioned by `at`** in `0003_partitions.sql:32-49`, with partitions only through
    `audit_log_2026_06` (covers → 2026-07-01).
- **Read-path enforcement already done (M3).** `storage.Resolve` switches on `blob_state`
  (`pkg/coordinator/storage/blob.go:95-106`): `quarantined → ErrBlobQuarantined`, `tombstoned →
  ErrBlobTombstoned`, `soft_deleted → ErrBlobSoftDeleted`; a `shredded` DEK → `ErrKeyShredded`
  (`blob.go:152`). The sentinels live in `storage/errors.go:21-30`. `handlers.mapBytesError`
  (`internal/api/handlers/blob.go:39-54`) maps `quarantined → 451 "content under moderation hold"` and
  `tombstoned`/`soft_deleted`/`key shredded → 410 "gone"`. **M9 sets states; it does not touch the read path**
  (it only adds the blocklist sentinel).
- **Cascade seam present (M5).** `product.Product.OnDelete(ctx, tx pgx.Tx, parentCID, newState string)`
  (`pkg/coordinator/product/interface.go:30`). nova-image's impl is the generic state cascade
  `UPDATE blobs SET state=$1 WHERE parent_cid=$2` (`nova-image/imageproduct/product.go:167-170`) — and its
  own comment confirms *"Child DEK shredding on tombstone is the core's job in M9 (it enumerates blobs by
  parent_cid)."* So OnDelete cascades **state**; M9's core enumerates and shreds derivative **DEKs**.
- **Primitives present.** `backend.Unpin(ctx, c cid.Cid) error` (`internal/ipfs/backend.go:80`);
  `InsertRevocation` (`internal/db/queries/signedurl.sql:33`) and the in-process revocation cache + refresh
  (M7, `RefreshEvery`); the zero-bytes crypto-shred pattern (`ShredExpiredRetiredSigningKeys`,
  `signedurl.sql:22`, invoked from `gcLoop` with a zero buffer); `httputil.ParsePage` + `Pagination` (M8,
  `internal/api/httputil/pagination.go`); the jobs queue's `WithNotBefore` (`internal/jobs/types.go:54`) —
  the deliberately-rejected alternative to the sweep.
- **Wiring pattern.** Subsystems are built in `coordinator.New` gated on `pool`/`backend`/`ks` (storage at
  `coordinator.go:191`; SigningAdmin at `:244`; the M8 audit stack at `:251-265`) and started as goroutines
  in `Run` beside `revocations.RefreshEvery`, `gcLoop`, `workers.Run`, and the audit maintainer/scheduler
  (`coordinator.go:438-462`). `gcLoop` (`:487`) is the precedent for a periodic in-process housekeeping loop.
  The admin group (`internal/api/server.go:128-143`, `RequireRole("operator","moderator")`) mounts nil-able
  `ServerConfig` handler pointers and falls through to `adminNotFound`. `cmd/coordinator/main.go` is
  env-driven and populates feature defaults onto `coordinator.Config` (`:198-203` for M8).
- **CLI pattern.** `cmd/novactl/main.go` is a hand-rolled `flag`-based dispatcher with `auth` and
  `signed-url` groups, a `~/.config/nova/credentials.json` bearer cache, and `postJSON`/`getJSON` helpers —
  the shape `novactl moderation` follows.
- **Test harness.** `internal/dbtest` (Postgres testcontainer), `internal/blobfixture` (seed blobs with
  known plaintext/visibility), and the nginx-fronted integration pattern
  (`internal/integration/m8_integrity_audit_test.go`, `m7_signed_urls_test.go`).

## Architecture

```
coordinator.New (pool + backend present)
   ├─ auditlog.NewWriter(gen.New(pool), slog)                       // two-mode Writer
   ├─ moderation.NewService(q, pool, backend, cascade, auditW, slog, clock)
   ├─ moderation.NewSweeper(svc, interval≈1m, slog)                 // in-process loop
   ├─ handlers.NewModerationAdmin(svc, q) / NewDMCAIntake(q) / NewAuditLogAdmin(q)
   ├─ extend auditMaintainer → also create-ahead audit_log partitions
   └─ pass auditW (best-effort) into NewSigningAdminHandler          // M7 backfill
   // no blocklist object: storage.{Resolve,Put} call q.IsBlocklisted directly

coordinator.Run
   └─ go moderationSweeper.Run(ctx)        // beside revocations refresh / gcLoop / workers / audit maintainer

Service.Quarantine / Tombstone / ClearLegalHold / Restore / CounterNotice
   tx := pool.Begin
   … state + DEK + decision + revocation + case + strike mutations …
   auditW.WriteTx(ctx, tx, Entry{…})       // audit is atomic with the action
   tx.Commit
   (Tombstone only) best-effort backend.Unpin(parent, derivatives…)   // after commit

Sweeper.tick (≈1m):
   rows := ListOverdueTombstones()         // scheduled_tombstone_at < now, quarantined, legal_hold=false
   for each: Service.Tombstone(cid, actor=nil)   // CHECK is the backstop
```

The moderation core **owns the write side of the lifecycle** the M3 read path already understands. It
deliberately performs its mutations through low-level sqlc queries inside one `pgx.Tx` rather than through
`storage.Put`/`Resolve`: those are built for *outbound serving* (visibility gating, encryption), whereas a
takedown must mutate state for *any* blob regardless of visibility and must compose several writes
atomically. This mirrors M8's decision to bypass the authz-gated read path for a coordinator-internal job.

### Package boundaries

| Package | Responsibility | Depends on |
|---|---|---|
| `internal/moderation` | `Service` (5 tx ops), `Sweeper`, domain errors, `CascadeHook`/`Writer` seams | `db/gen`, `pgxpool`, `ipfs.Backend`, `auditlog`, `go-cid`, `log/slog` |
| `internal/auditlog` | two-mode `Writer` (`WriteTx`/`Write`) + `Entry` over `audit_log` | `db/gen`, `pgx`, `log/slog` |
| `internal/db` (`moderation.sql`,`audit.sql` → `gen`) | decisions, state/DEK mutations, cases, blocklist, infringer, audit, overdue-sweep | — |
| `internal/api/handlers` | `ModerationAdmin`, `DMCAIntake`, `AuditLogAdmin` | `db/gen`, `moderation`, `bearer`, `httputil` |
| `internal/api` (`server.go`) | mount public `/legal/dmca` + admin routes; new `ServerConfig` pointers | `handlers` |
| `pkg/coordinator` | build the Service/Sweeper/handlers; `productHook.OnDelete` cascade adapter; start the sweep | `moderation`, `auditlog`, `ipfs` |
| `cmd/coordinator` | sweep-interval default + optional enable toggle | `coordinator` |
| `cmd/novactl` | `moderation` subcommand group (HTTP client) | — |

`internal/moderation` imports only lower-level packages and **not** `pkg/coordinator/product` (it defines a
`CascadeHook func(ctx, tx, parentCID, newState string) error` the coordinator wires from `product.OnDelete`,
inverting the dependency exactly as `storage.WriteHook` does) and **not** `internal/jobs` (the sweep is
in-process; it never enqueues persistent work). `internal/auditlog` is a leaf the Service and the M7 admin
handler both depend on.

## The moderation lifecycle and transactions

The lifecycle is the `blob_state` machine `DMCA_PROCEDURE.md` describes:

```
active ──quarantine──▶ quarantined ──(sweep: scheduled_tombstone_at elapsed, legal_hold=false)──▶ tombstoned
                            │  ▲                                                                      (DEK shredded)
              counter-notice│  │restore                          clear-legal-hold sets
              (clear sched.)│  │(→ active)                       scheduled_tombstone_at=now()
                            ▼  │                                  so the next sweep tombstones
                        quarantined (held)
   severe: quarantine --legal-hold ⇒ quarantined + DEK.legal_hold=true + scheduled_tombstone_at=NULL
           (tombstone refused by no_shred_under_legal_hold until clear-legal-hold)
```

Every operation runs in a single `pgx.Tx` and writes its `audit_log` row via `auditW.WriteTx` **inside** that
tx — so the action and its audit record commit or roll back together. The `audit_log.action` strings follow
`DMCA_PROCEDURE.md` § "Audit trail" (`dmca.quarantined`, `dmca.tombstoned`, `dmca.restored`,
`dmca.counter_received`, `severe.quarantined`, `severe.legal_hold_cleared`).

- **`Quarantine(cid, rule, ruleRef, reason, tombstoneAfter, legalHold, actor)`** — `rule ∈
  {dmca, severe_content, operator_manual}`:
  1. `INSERT moderation_decisions(cid, rule, rule_ref=ruleRef, action='quarantine', decided_by=actor,
     scheduled_tombstone_at = legalHold ? NULL : now()+tombstoneAfter, legal_hold=legalHold, notes=reason)`.
  2. `SetBlobState(cid, 'quarantined')` for the parent; `CascadeHook(tx, cid, 'quarantined')` for derivatives.
  3. If `legalHold`: `SetDEKLegalHoldByBlob(cid, true)` for the parent and each derivative.
  4. `InsertRevocation('cid', cid)` (idempotent under the `UNIQUE(kind,value)` constraint; outstanding signed
     URLs fail within the M7 revocation-refresh window).
  5. If `ruleRef` names a DMCA case: `SetDMCACaseActioned(ruleRef)`.
  6. `UpsertRepeatInfringer(ownerID)` (skip when the blob has no owner).
  7. `WriteTx` (`dmca.quarantined` / `severe.quarantined`), `payload` carrying case id, reason,
     `scheduled_tombstone_at`, `legal_hold`.
- **`Tombstone(cid, rule, ruleRef, reason, actor)`** — manual `takedown` or the sweep:
  1. `INSERT moderation_decisions(action='tombstone', …)`.
  2. **`ClearScheduledTombstone(originating quarantine decision)`** — sets its `scheduled_tombstone_at = NULL`
     so the finished row leaves `moderation_decisions_scheduled_tombstone_idx` and the sweep stays
     `O(pending)` (see Risks / the index note).
  3. `SetBlobState(cid, 'tombstoned')`; `CascadeHook(tx, cid, 'tombstoned')`.
  4. **Shred the DEKs.** `ShredDEKByBlob(cid, zeros)` for the parent and, via `ListDerivativeCIDs(cid)`, each
     derivative — `UPDATE data_encryption_keys SET state='shredded', wrapped_key=$zeros, shredded_at=now()
     WHERE id = (SELECT encryption_key_id FROM blobs WHERE cid=$1) AND encryption_key_id IS NOT NULL`. If any
     target carries `legal_hold=true` the `no_shred_under_legal_hold` CHECK raises; the Service maps the
     constraint error to `ErrLegalHold` and the **whole tx rolls back** (nothing tombstoned). This is exit
     criterion #2.
  5. `InsertRevocation('cid', cid)`; `SetDMCACaseActioned` if a case; `UpsertRepeatInfringer`.
  6. `WriteTx` (`dmca.tombstoned`). Commit.
  7. **After commit:** best-effort idempotent `backend.Unpin(parent)` and each derivative CID. A failure is
     logged, not fatal (the bytes are already cryptographically inert — see Risks for the orphan window).
- **`ClearLegalHold(cid, caseRef, reason, actor)` [operator-only]:** `SetDEKLegalHoldByBlob(cid, false)`
  (parent + derivatives); `UPDATE moderation_decisions SET legal_hold=false, scheduled_tombstone_at=now()
  WHERE cid=$1 AND legal_hold`; `WriteTx` (`severe.legal_hold_cleared`). The next sweep tombstones it (exit
  criterion #3). This is the `SEVERE_CONTENT_PROCEDURE.md` § "Operator legal-hold-clear" SQL.
- **`Restore(cid, reason, actor)`:** `SetBlobState(cid, 'active')`; `CascadeHook(tx, cid, 'active')`;
  `ClearScheduledTombstone(cid)`; `WriteTx` (`dmca.restored`). Quarantine-only (a tombstone is final). **It
  does not delete the `('cid', cid)` revocation** — see the box below.
- **`CounterNotice(caseID|cid, notes, actor)`:** `ClearScheduledTombstone` for the cid; `AppendDMCACaseNotes`;
  `WriteTx` (`dmca.counter_received`). The blob stays `quarantined` (reversibility preserved through the
  window); the operator later `Restore`s or lets a renewed schedule tombstone.
- **`Sweeper.tick`** (≈1 min): `ListOverdueTombstones()` returns decisions with `scheduled_tombstone_at <
  now()`, `action='quarantine'`, the blob still `quarantined`, and the DEK `legal_hold=false`; the Service
  runs `Tombstone(cid, actor=nil)` for each. The CHECK is the backstop if a `legal_hold` row ever slips the
  filter. The loop honours an enable flag and stops on `ctx` cancellation; on restart it simply re-reads the
  table (no persisted in-flight state).

> **Restore does not clear the signed-URL revocation (review decision).** `signed_url_revocations` has no
> issuer/source column (`id, kind, value, revoked_at`; `UNIQUE(kind,value)`), so a quarantine-inserted
> `('cid', cid)` row is indistinguishable from one a security-minded operator inserted manually via the M7
> `signed-urls/revoke` endpoint. A blind delete on Restore could silently un-revoke that manual action.
> Leaving the revocation in place is also the safer posture: URLs exposed during the quarantine window should
> be re-minted, not auto-reactivated. Clearing a revocation stays an explicit, separate operator step. Adding
> a `source` column to `signed_url_revocations` is the precise-but-heavier alternative and is deferred.

## The `audit_log` writer (two-mode)

`internal/auditlog` owns the writer deferred from M6/M7. The table is partitioned (above), append-only at the
application layer (`DMCA_PROCEDURE.md` § "Audit trail" recommends revoking `UPDATE`/`DELETE` on it for the app
role in production).

```go
type Entry struct {
    ActorID    *uuid.UUID      // nil ⇒ system action (the sweep)
    Action     string          // "dmca.quarantined", "signing_key.rotated", …
    TargetType string           // "cid" | "dmca_case" | "signing_key"
    TargetID   string
    Payload    map[string]any   // → jsonb
}

type Writer interface {
    // Atomic — used by moderation, inside the action's tx (gen.New(pool).WithTx(tx)).
    // An audit failure rolls the action back: no orphan action, no orphan log.
    WriteTx(ctx context.Context, tx pgx.Tx, e Entry) error
    // Best-effort — used by backfilled M7 admin actions, AFTER they commit.
    // Never returns an error; never touches the caller's tx; failures are slog.Warn'd.
    Write(ctx context.Context, e Entry)
}
```

Two modes, chosen per call site for correctness, not convenience:

- **Moderation → `WriteTx`.** The DMCA runbook mandates the audit insert *inside* the state-change
  transaction; making it atomic is what guarantees "no takedown without a record, no record without a
  takedown."
- **M7 admin backfill → `Write`.** `signing-key rotate`, `signed-url revoke`, `signed-url sign` already
  commit their own work; the audit is attached best-effort afterward so a transient audit failure can never
  fail or slow an operator action that already happened. These are low-frequency privileged actions.
- **Routine auth is not audited** (scope, above) — that removes any login-path latency concern outright.
- With partition create-ahead in place, the insert effectively cannot fail for lack of a partition, so the
  best-effort path's failure mode is reduced to genuine DB outages (which the action itself would also hit).

`GET /api/v1/admin/audit-log` lists rows newest-first with `httputil` pagination and optional `action` /
`target_type` / `actor_id` filters.

## The CID blocklist

`0008_moderation.sql`:

```sql
CREATE TABLE blocklist (
    cid         text PRIMARY KEY,
    reason      text NOT NULL,
    rule        moderation_rule NOT NULL DEFAULT 'operator_manual',
    added_by    uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blocklist_created_at_idx ON blocklist (created_at DESC);
```

**Enforcement is a direct indexed lookup, not a cache (review decision).** `blocklist.cid` is the PRIMARY
KEY, so `IsBlocklisted(cid)` is an `O(log n)` index probe — sub-millisecond, scaling to millions of rows,
with no in-memory copy. This deliberately rejects the M7-style full-table `RefreshEvery` cache: a periodic
`SELECT *` over a table that could grow large is a CPU/IO/GC cliff (and the read path already does several
indexed queries, so one more PK probe is marginal). `storage.Service` already holds `gen.Queries`, so it
calls `s.q.IsBlocklisted` directly with no injected dependency, no refresh goroutine, and no staleness
window. A bounded LRU+TTL is the documented optimization *only if* a per-read probe ever shows up in
profiling (YAGNI now).

- **Read** — `storage.Resolve` calls `IsBlocklisted` and returns a new sentinel `ErrBlobBlocklisted`
  (`storage/errors.go`), mapped to `451` (defense-in-depth; a blocklisted CID that is also tombstoned would
  already 410).
- **Import/commit** — `storage.Put` checks the derived CID before committing/pinning and refuses
  (`openapi.yaml:589`, "rejected by the moderation blocklist"). This genuinely prevents re-upload of
  `public_archival` (unencrypted, deterministic-CID) content; for encrypted content it is a permanent
  exact-CID deny (the nonce-CID limit is in Risks).
- **Admin CRUD** — `GET/POST /api/v1/admin/moderation/blocklist`, `DELETE …/blocklist/{cid}`.

## Scheduled-tombstone sweep

`DMCA_PROCEDURE.md` § "Action" specifies *"a scheduled job runs every minute and tombstones overdue rows"*
and gives the join. M9 implements that as an **in-process `Sweeper`**, a sibling of `gcLoop` and the M8
`Maintainer`, not as `jobs` rows:

- **Why in-process.** Counter-notice is *"clear `scheduled_tombstone_at`"* — trivial against a table the
  sweep re-reads each tick, but awkward against an enqueued `jobs` row that would have to be found and
  cancelled. The candidate set is tiny and selective (the partial index), the work is idempotent, restart
  needs no persisted state, and the read-light loop stays decoupled from the write-heavy `jobs.Queue`. The
  `jobs`+`WithNotBefore` alternative was considered and rejected for the cancellation coupling.
- **Index discipline.** The sweep is served by the existing partial index
  `moderation_decisions_scheduled_tombstone_idx (scheduled_tombstone_at) WHERE scheduled_tombstone_at IS NOT
  NULL` (`0001_init.sql:526`); the query carries an explicit `scheduled_tombstone_at IS NOT NULL` so the
  partial index is used (no sequential scan). **No new index is added.** A redundant `… WHERE
  action='quarantine'` index would not help — a completed auto-tombstone keeps `action='quarantine'` on its
  originating row — so the safeguard that keeps the candidate set bounded is the tombstone step that **clears
  `scheduled_tombstone_at`**, evicting finished rows from the partial index. Without that, every auto-
  tombstoned row would linger in the index and be re-scanned every minute forever.
- **Cadence + control.** `interval` defaults to ~1 minute (configurable); an `Enabled` flag gates the loop
  (the `Maintainer`-style "always-safe to disable" knob); per-tick work is bounded by the small candidate set.

## HTTP contract

### Public

| Method | Path | Auth | Notes |
|---|---|---|---|
| POST | `/legal/dmca` | none (rate-limited) | § 512(c)(3) body → `dmca_cases(status='received')`; `202` + `{case_id}`. No action. |

### Admin (`/api/v1/admin/*`, `RequireRole("operator","moderator")`)

| Method | Path | Extra guard | Result |
|---|---|---|---|
| POST | `/moderation/quarantine` | — | `200`; body `{cid, rule?, case_id?, reason, tombstone_after?, legal_hold?}` |
| POST | `/moderation/takedown` | — | `200`; immediate tombstone + shred (moderators "execute takedowns", `user_role` enum) |
| POST | `/moderation/clear-legal-hold` | `operator` | `200`; body `{cid, case_ref, reason}` — the one operator-only action |
| POST | `/moderation/restore` | — | `200`; body `{cid, reason}` |
| POST | `/moderation/counter-notice` | — | `200`; body `{case_id?|cid, notes}` |
| GET | `/moderation/queue` | — | paginated `moderation_decisions` |
| GET | `/moderation/dmca`, `/moderation/dmca/{id}` | — | DMCA cases (list / detail) |
| GET/POST | `/moderation/blocklist` | — | list / add `{cid, reason}` |
| DELETE | `/moderation/blocklist/{cid}` | — | remove |
| GET | `/audit-log` | — | paginated; filters `action`, `target_type`, `actor_id` |

`clear-legal-hold` is the only action narrowed to `operator` (via `r.With(RequireRole("operator"))`), per
`SEVERE_CONTENT_PROCEDURE.md`; every other action stays at the group's `operator`+`moderator` guard, since
the `user_role` enum (`0001_init.sql:56`) makes executing takedowns a moderator capability. The destructive
verbs are gated instead by the CLI's confirmation prompt. `ServerConfig` gains nil-able `ModerationAdmin`,
`DMCAIntake`, `AuditLogAdmin` (each `nil ⇒` the path falls through to `adminNotFound` / is unmounted, exactly
like `SigningAdmin`/`AuditAdmin`). `/legal/dmca` mounts beside `/blob` as a public route.

### Error → status

| Condition | Status | `code` |
|---|---|---|
| no bearer on an admin route | 401 | `unauthenticated` |
| authenticated, wrong role | 403 | `forbidden` |
| tombstone/shred blocked by legal hold (`ErrLegalHold`) | 409 | `legal_hold` |
| blob not quarantined for restore/counter (`ErrNotQuarantined`) | 409 | `conflict` |
| unknown CID / case | 404 | `not_found` |
| bad body / pagination / filter | 400 | `invalid_request` |
| blocklisted on import | 451 | `blocklisted` |
| ok | 200 / 202 | — |

## CLI: `novactl moderation`

A thin HTTP client over the admin API (mirrors `cmdSignedURLSign` — bearer from
`~/.config/nova/credentials.json`, `postJSON`/`getJSON`):

```
novactl moderation quarantine <cid> [--case <id>] [--reason <s>] [--tombstone-after 14d] [--legal-hold]
novactl moderation takedown   <cid> [--case <id>] [--reason <s>] [--no-confirm]
novactl moderation clear-legal-hold <cid> [--case-id <ref>] [--reason <s>] [--no-confirm]
novactl moderation restore    <cid> [--reason <s>]
novactl moderation list       [--state quarantined] [--per-page N]
```

Destructive verbs (`takedown`, `clear-legal-hold`) prompt for confirmation unless `--no-confirm`
(`DMCA_PROCEDURE.md` § "Action (immediate-tombstone)"). `usage()` and the package doc comment are updated.

## Configuration

```go
// pkg/coordinator/coordinator.go
type ModerationConfig struct {
    SweepEnabled  bool          // master switch (default true)
    SweepInterval time.Duration // default ~1 min; 0 ⇒ default
}
```

`coordinator.New` builds the moderation stack only when `pool + backend` are present (the sweep tombstones
and `Unpin`s, both of which need the backend). `cmd/coordinator/main.go` sets the default interval and reads
an optional `NOVA_MODERATION_SWEEP_ENABLED` (`"false"` disables the sweep; the rest of moderation — the
admin API, intake, blocklist — still works). No per-action env knobs; `operator.yaml` decode stays deferred.

## Startup and wiring

- **`coordinator.New`** (when `pool+backend`): construct `auditlog.Writer(gen.New(pool))`; the
  `moderation.Service` over `gen.New(pool)` + `pool` + `backend` + a `CascadeHook` adapter (a new
  `productHook.OnDelete(ctx, tx, parentCID, newState)` that dispatches to the registered product, no-op when
  none) + the `Writer`; the `Sweeper`; set `sc.ModerationAdmin/DMCAIntake/AuditLogAdmin`; pass the `Writer`
  into `NewSigningAdminHandler` for the M7 backfill; extend the audit `Maintainer` to also create-ahead
  `audit_log` partitions. Storage gains no new dependency — it calls `q.IsBlocklisted` directly.
- **`coordinator.Run`:** `go moderationSweeper.Run(ctx)` beside the existing loops.
- **No new refuse-to-start.** A disabled sweep simply never ticks; everything else degrades gracefully when
  `backend`/`pool` is absent (test coordinators), matching M7/M8.

## Partition create-ahead for `audit_log`

`audit_log` is monthly-partitioned with committed partitions only through `audit_log_2026_06` (→ 2026-07-01);
a partitioned table rejects out-of-range inserts, so the writer would begin failing ~a month out. M8's
`Maintainer` already solves this for `integrity_audits`; M9 **extracts the monthly create-ahead into a small
helper and calls it for `audit_log` too** (current month + next two), at boot and on the existing 24 h tick.
`audit_log` is **not pruned** (legal retention is years — `DMCA_PROCEDURE.md` § "Record retention"
recommends ≥ 7 y); only `integrity_audits` keeps its M8 prune/drop policy. Partition names derive from month
boundaries (never user input), so the DDL stays `fmt.Sprintf` + `pool.Exec`, as in M8.

## Security and privacy considerations

- **Crypto-shred is the durable control; the CHECK is the floor.** Tombstone zeroes `wrapped_key` and sets
  `state='shredded'`, rendering the ciphertext mathematically inert. `no_shred_under_legal_hold` makes that
  impossible while `legal_hold=true`, enforced at the storage layer regardless of any application bug — the
  property `SEVERE_CONTENT_PROCEDURE.md` depends on.
- **Audit atomicity.** Moderation audit rows commit with the action (`WriteTx`); the trail cannot diverge
  from reality. The log records `actor_id`, never secrets; `payload` carries case/claimant context, never key
  material or plaintext.
- **Reads fail closed.** Quarantine → 451, tombstone/shred → 410, blocklist → 451 — all already through the
  M3 read path's sentinels; signed URLs for a quarantined CID fail within the M7 refresh window via the
  inserted revocation.
- **DMCA intake is public and untrusted.** `/legal/dmca` only *records* a case (no action, no content
  echo); it is rate-limited by the existing middleware and stores exactly the § 512(c)(3) fields.
- **No new IP/identity retention.** Moderation rows key on `cid`/`case`/`user`; paranoid mode is unaffected.

## Testing strategy

### Unit (`internal/moderation`, `internal/auditlog`, handlers)

- **Quarantine**: sets `state='quarantined'`, cascades a seeded derivative, inserts the decision + the
  `('cid',cid)` revocation + the owner strike, and writes the audit row — all visible after commit; with
  `--legal-hold`, the DEK(s) flip `legal_hold=true` and `scheduled_tombstone_at` is `NULL`.
- **Tombstone**: shreds the parent **and** derivative DEKs (`wrapped_key` zeroed, `state='shredded'`),
  clears the originating `scheduled_tombstone_at`, unpins (fake backend records the calls); **tombstone of a
  `legal_hold=true` blob returns `ErrLegalHold` and rolls back** (state unchanged, DEK intact) — the
  `no_shred_under_legal_hold` violation asserted directly.
- **ClearLegalHold → Tombstone**: after clear, the same tombstone now succeeds (exit criterion #3).
- **Restore**: re-activates parent + derivatives, clears the schedule, and **leaves the revocation intact**
  (asserted).
- **Sweeper**: tombstones only overdue, still-quarantined, non-legal-hold rows; a legal-hold row is skipped;
  a disabled sweep never ticks; restart re-reads the table.
- **Blocklist**: `IsBlocklisted` hit/miss; `Resolve` → `ErrBlobBlocklisted` (451); `Put` refuses a
  blocklisted `public_archival` CID.
- **AuditWriter**: `WriteTx` is atomic (a forced failure rolls the action back); `Write` swallows + logs a
  failure and never returns an error.
- **Handlers**: pagination/filter parsing; the authz matrix; `ErrLegalHold→409`, `404`/`400` mappings.

### Integration (`internal/integration/m9_moderation_test.go`, nginx-fronted, testcontainers)

End-to-end against the untagged coordinator behind nginx (Postgres + nginx containers), in the M7/M8 shape:

1. Boot with a fast sweep; create `operator`/`moderator`/`uploader`; upload an encrypted blob (+ an image
   derivative) and a `public_archival` blob.
2. **Exit #1:** `POST /legal/dmca` → `GET /moderation/dmca` shows the case; `POST /moderation/quarantine`
   (`--case`, short `tombstone-after`) → `GET /blob/{cid}` returns `451`; wait for the sweep → `GET` returns
   `410`, the DEK is `shredded`, and the fake/real backend shows the CID unpinned; `GET /audit-log` shows
   `dmca.quarantined` then `dmca.tombstoned`.
3. **Exit #2:** `POST /moderation/quarantine --legal-hold` → `POST /moderation/takedown` returns `409
   legal_hold`; the DEK is still `active`.
4. **Exit #3:** `POST /moderation/clear-legal-hold` (operator) → sweep → `410` + `shredded`.
5. **Blocklist:** add the `public_archival` CID → re-import is refused (`451`); read returns `451`.
6. **Authz:** operator/moderator get the queue/audit-log (`200`); no token `401`; `uploader` `403`;
   `clear-legal-hold`/`takedown` reject `moderator` (`403`).
7. The extended Maintainer created next month's `audit_log` partition (assert it exists).

### CI

- `make codegen-check` after regenerating from `moderation.sql`/`audit.sql`.
- `-short`-skippable integration, like M2–M8; full run in the integration job.
- gofmt only the files M9 touches (toolchain-skew rule); `golangci-lint` is CI-only.

## File structure

### Created in M9

```
internal/db/migrations/0008_moderation.sql        blocklist table (+ DATA_MODEL.sql mirror)
internal/db/queries/moderation.sql                decisions, state/DEK mutations, cases, blocklist, infringer, overdue-sweep
internal/auditlog/writer.go                        two-mode Writer + Entry
internal/auditlog/writer_test.go
internal/moderation/service.go                     Quarantine/Tombstone/ClearLegalHold/Restore/CounterNotice + errors + CascadeHook seam
internal/moderation/service_test.go
internal/moderation/sweeper.go                     in-process ≈1m loop
internal/moderation/sweeper_test.go
internal/api/handlers/moderation_admin.go          /api/v1/admin/moderation/* (actions, queue, cases, blocklist)
internal/api/handlers/moderation_admin_test.go
internal/api/handlers/dmca_intake.go               public POST /legal/dmca
internal/api/handlers/audit_log_admin.go           GET /api/v1/admin/audit-log
internal/api/handlers/audit_log_admin_test.go
internal/integration/m9_moderation_test.go         end-to-end through nginx (the three exit criteria + blocklist + audit-log)
```

### Modified in M9

```
internal/db/queries/audit.sql                      + InsertAuditLog / ListAuditLog / CountAuditLog
internal/db/gen/*                                  regenerated
internal/db/migrations/.../DATA_MODEL.sql          + blocklist; audit_log create-ahead note
internal/audit/integrity/retention.go              extract monthly create-ahead helper; cover audit_log
internal/api/server.go                             mount /legal/dmca + /admin/moderation/* + /admin/audit-log; ServerConfig pointers
internal/api/handlers/signing_admin.go             best-effort audit Write on rotate/revoke/sign (M7 backfill)
pkg/coordinator/coordinator.go                     ModerationConfig; build Service/Sweeper/handlers; productHook.OnDelete; start sweep
pkg/coordinator/producthook.go                     OnDelete cascade adapter
pkg/coordinator/storage/blob.go                    IsBlocklisted checks in Resolve + Put; ErrBlobBlocklisted
pkg/coordinator/storage/errors.go                  + ErrBlobBlocklisted
cmd/coordinator/main.go                            ModerationConfig defaults; NOVA_MODERATION_SWEEP_ENABLED
cmd/novactl/main.go                                moderation subcommand group + usage
docs/specs/openapi.yaml                            reconciliations #1
docs/legal/DMCA_PROCEDURE.md                       reconciliation #2
docs/legal/SEVERE_CONTENT_PROCEDURE.md             reconciliation #3
docs/specs/INTEGRITY_AUDIT.md                      reconciliation #5
docs/ROADMAP.md                                    reconciliation #6 (status + tag + deferrals)
```

### Reused unchanged

```
pkg/coordinator/storage/blob.go (Resolve state switch)   M3 quarantined/tombstoned mapping
internal/api/handlers/blob.go (mapBytesError)            451/410 already wired
pkg/coordinator/product (OnDelete contract)              M5 cascade seam
internal/db/queries/signedurl.sql (InsertRevocation)     M7 cid revocation
internal/ipfs/backend.go (Unpin)                         local unpin
internal/api/httputil (ParsePage, Pagination, WriteError) M8 pagination + error shape
internal/auth/bearer (RequireRole)                       admin guards
internal/dbtest, internal/blobfixture                    test harness
```

## Risks and notes

- **Kubo unpin-after-commit window → orphaned pinned blocks (decided: documented tech debt).** Tombstone
  commits the crypto-shred (DEK zeroed → bytes are cryptographically inert *immediately*; this is **not** a
  confidentiality leak), then best-effort `Unpin` outside the tx. A crash in that window leaves the
  ciphertext blocks pinned with no DB owner → a silent **disk-reclamation** leak (Kubo GC only sweeps
  unpinned blocks). DB-commit-then-unpin is the correct order — the DB is the source of truth, and reversing
  it would risk unpinning a live blob on rollback. Owner: a **Phase-5 hardening reconciliation pass** that
  diffs the local Kubo pinset against `blobs`/`blob_blocks` and unpins orphans — the natural home is the M8
  integrity-audit framework (an inverse of `kubo_pin_present`). `Unpin` is idempotent, so an optional cheap
  M9 mitigation — a one-shot startup re-unpin of `state='tombstoned'` blobs still pinned — can shrink the
  window; the comprehensive sweep stays deferred.
- **Blocklist re-upload limit (decided).** Envelope CIDs are nonce-randomized, so the CID blocklist cannot
  recognise a re-encrypted copy of the same image (fresh CID each upload). It is effective for
  `public_archival` (deterministic CID) and as a permanent exact-CID deny; visual re-upload prevention needs
  perceptual hashing — Phase 3. The openapi is reconciled to this reality (#1).
- **Sweep candidate-set growth (mitigated).** Clearing `scheduled_tombstone_at` on tombstone keeps the
  partial index — and thus the per-minute sweep — `O(pending)`; without it, finished rows would accumulate
  in the index. No redundant index is added (it would not help — see Scheduled-tombstone sweep).
- **`signed_url_revocations` has no issuer column (decided).** Restore leaves revocations intact rather than
  risk clobbering a manual operator revocation; a `source` column is the deferred precise fix.
- **Repeat-infringer is accounting only.** M9 increments strikes and surfaces them; auto-suspension needs an
  account-state model the `users` table lacks — deferred.

## Cross-references

- `docs/legal/DMCA_PROCEDURE.md` — normative quarantine/tombstone transactions, sweep SQL, counter-notice,
  repeat-infringer accounting, audit-trail vocabulary.
- `docs/legal/SEVERE_CONTENT_PROCEDURE.md` — Phase-1 manual `--legal-hold` + `clear-legal-hold`; the
  `no_shred_under_legal_hold` rationale.
- `docs/specs/DATA_MODEL.sql` — `blob_state`, `moderation_*`, `dmca_*`, `data_encryption_keys` + the CHECK,
  `audit_log`, `signed_url_revocations`; `internal/db/migrations/0003_partitions.sql` — `audit_log`
  partitioning.
- `docs/specs/openapi.yaml` — `/legal/dmca`, `/api/v1/admin/moderation/*`, `/api/v1/admin/audit-log`
  (reconciled to CID blocklist).
- `docs/specs/ENCRYPTION_ENVELOPE.md` § "Crypto-shredding" — the DEK-zeroing semantics the tombstone
  performs; `docs/specs/INTEGRITY_AUDIT.md` — `derivative_state_consistent`, now policing real cascades.
- M8 design (`2026-06-02-phase1-m8-integrity-audit-scheduler-design.md`) — the in-process loop, the
  `Maintainer`/partition pattern M9 extends, the pagination helper, and the admin-route + `ServerConfig`
  conventions M9 reuses.
- `docs/superpowers/plans/2026-06-02-phase1-m9-moderation.md` — the implementation plan.
```
