# Task 14b — P2-M4.1 read-redirect E2E composition test

**Status: DONE**

## What was built

A single end-to-end composition test, `TestM41ReadRedirectE2E`, in
`pkg/coordinator/m41_readredirect_e2e_test.go` (package `coordinator_test`, the
external test package). Placement is deliberate: `pkg/coordinator` is top-level
(imported by nothing), so it may import `storage`, `admission`,
`internal/node/source`, `internal/federation/{ca,tokens,transport,wire,replay}`,
`internal/node/bandwidth`, `internal/db/gen`, `internal/dbtest` freely. The same
test in `internal/federation/coordinator` would be an import cycle (admission +
storage import that package).

Plus two minimal exported one-shot wrappers in the `storage` package (the only
production change), thinly delegating to the existing unexported methods and
documented as test/ops seams:

- `func (r *Reconciler) ReconcileOnce(ctx context.Context)` → `r.reconcileOnce(ctx)`
  (`pkg/coordinator/storage/reconciler.go`)
- `func (p *Pruner) PruneOnce(ctx context.Context)` → `p.pruneOnce(ctx)`
  (`pkg/coordinator/storage/prune.go`)

## Real wire components exercised

- Real `storage.Service` built via `NewService` with the production options:
  `WithProductHook`, `WithStorageMode(bounded_cache, MaxBytes=4096)`,
  `WithCommitGate(RequireQuorum, important=2)`, `WithAssigner(admission.New)`,
  `WithDonorReadSource(coordClientTLS, signer)`.
- Real `storage.NewReconciler` / `storage.NewPruner`, driven via the new
  one-shot wrappers.
- Real `admission.Assigner` (drives `coordinator.AssignPin` against the DB).
- Real Postgres (testcontainer) with all 13 migrations.
- Real envelope keystore (`NewKeystoreFromEnv` + `Bootstrap`).
- TWO real donor read-source servers (`source.NewServer`) over loopback **mTLS**:
  real CA (`ca.GenerateCA`), real coordinator client cert
  (`ca.IssueCoordinatorClientCert` → `transport.CoordinatorClientTLS`), real
  donor server certs (`ca.IssueServerCert` → `transport.ServerTLSConfig`),
  `transport.NewTLSListener` + `http.Serve`. The coordinator's
  `httpDonorFetcher` does a genuine GET over its mTLS client identity.
- The donor's full verify chain runs for real: coordinator role from the peer
  cert, signed read-grant verify (`wire.Verify` against the shared Ed25519
  pubkey), source/dest/cid binding, local progress = acked-delivered with
  matching assignment_id/generation/byteSize, boot-floor + single-use replay
  (`replay.New`), pin check, envelope-size preflight, and the D11 egress debit
  (`bandwidth.NewDailyBucket`).
- The coordinator's **verify-before-serve** gate is genuine: the donor bytes are
  re-imported via the in-memory backend's `AddDeterministic`, which computes a
  real `CIDv1(raw, sha2-256)` (multihash.Sum); a CID mismatch would be a real
  rejection, not a stub.

## Coverage map

### Asserted END-TO-END here (all 5 brief steps)

1. **Gate-on Put ⇒ staging, invisible, no OnCommitted.** Real `svc.Put`
   (gate-on) over a public_archival collection → `DurabilityState=="staging"`;
   `svc.Resolve` ⇒ `errors.Is(err, storage.ErrStagingNotVisible)`; the recording
   hook's `OnCommitted` count is 0.
2. **ReconcileOnce ⇒ committed + hook once + visible.** After seeding 2 acked
   sourceable donor holders, `reconciler.ReconcileOnce` flips commit_state to
   `committed`, fires `OnCommitted` exactly once, and `svc.Resolve` now succeeds.
3. **PruneOnce ⇒ origin unpinned.** With sourceable=2 ≥ floor=2,
   `pruner.PruneOnce` makes `backend.Has(cid)` go from true to false.
4. **Cold OpenBytes ⇒ real donor fetch over mTLS + verify + correct bytes;
   warm read = local cache hit.** Cold `svc.OpenBytes` fetches the envelope from
   a real donor read-source server, verifies (CID re-import match), and returns
   the original plaintext. A per-donor served-request counter confirms exactly
   one donor served the cold read; a second `OpenBytes` does NOT advance the
   counter (genuine local re-cache hit).
5. **No reachable holder ⇒ ErrNoSourceableHolder (503), not 404.** Both donor
   servers are closed and their nodes made non-sourceable; the locally re-cached
   copy is evicted to force the donor path; `svc.OpenBytes` ⇒
   `errors.Is(err, storage.ErrNoSourceableHolder)` and `NOT
   errors.Is(err, storage.ErrBlobNotFound)`.

### Covered by component tests (not re-proven here)

- Donor serve + full verify chain + egress budget: Task 6 (`internal/node/source`).
- Coordinator fetch + verify-before-decrypt + reputation-ordered fallback +
  404/503 sentinels + single-flight + breaker + bounded fallback: Tasks 7/8
  (`pkg/coordinator/storage/readsource_test.go`).
- Bounded-cache SLRU admit/promote/evict byte-budget arithmetic: Task 9
  (`cache_test.go`).
- Reconciler commit-on-quorum / fail-on-age / idempotency: Task 11
  (`reconciler_test.go`).
- Pruner at/below floor, crash-window reconcile, class handling: Task 12
  (`prune_test.go`).
- Transform re-fetch + staging guard (brief step 7, image-product transform):
  Task 13 — explicitly OUT of this E2E per the task scope.

### Deferred / intentional simplifications (why)

- **Blob is unencrypted (public_archival), not an encrypted envelope.** This
  keeps the assembly reliable (no DEK row + keystore decrypt threading) while
  still exercising the load-bearing M4.1 invariant — donor serves bytes →
  coordinator verifies (AddDeterministic CID match) BEFORE serving. The
  verify-before-serve gate is byte-identical to the encrypted path
  (`attemptHolder` runs the same re-import for both); only the final
  decrypt-vs-passthrough differs, and the decrypt path is already covered by
  `TestOpenBytesEncrypted` (Task 7) and `put_test.go`'s encrypted round-trip.
- **The M4-proven agent replicate→ack loop is skipped; acked holders are
  DB-seeded directly** (as the brief instructs). The novel M4.1 read path is
  what runs live.
- **In-memory `ipfs.Backend`** (no real Kubo) for the coordinator origin store —
  but with a real `CIDv1(raw, sha2-256)` so verification is genuine. Matches the
  `echoBackend`/`fakeBackend` pattern used throughout the storage package tests.

## Build / test evidence

- `go build ./...` — clean.
- `go test ./pkg/coordinator/ -run TestM41ReadRedirectE2E -v` — PASS (~4.9s).
- `go test ./pkg/coordinator/...` — PASS (coordinator 17.7s, admission 17.7s,
  product 0.02s).
- `go test ./pkg/coordinator/storage/...` — PASS (142.5s) — the exported
  wrappers do not break any existing test.
- `gofmt -l` on the three touched files — empty (clean).
- `bash scripts/check_node_deps.sh` — `OK: cmd/node dependency boundary clean`.
- `bash scripts/check-migrations-frozen.sh` — exit 0.

## Concerns

None blocking. One note: the test is DB-backed (testcontainers/Docker) and
gated on `testing.Short()`, consistent with the other storage integration
tests. Runtime ~5s standalone. No migration, no schema change, no
production-behavior change beyond the two thin exported wrappers.
