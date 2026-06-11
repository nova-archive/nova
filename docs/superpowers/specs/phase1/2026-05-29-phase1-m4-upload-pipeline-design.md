# Phase 1 M4 — Upload Pipeline (Write Path) Design

**Goal:** A coordinator process serves the anonymous **write path** end-to-end: a
curl-driven `tus` resumable upload **or** a `multipart/form-data` upload encrypts the
plaintext, imports the envelope to embedded Kubo deterministically, commits the blob +
manifest + block + key rows in a single transaction (unpinning the orphan import on any
rollback), and returns a CID whose bytes are immediately fetchable via M3's read path.

**Architecture (one line):** `internal/api` upload handlers drive an HTTP-naïve
`internal/upload` tus session store (Postgres `upload_sessions` row + on-disk chunk file)
and, at finalize/multipart, call a product-agnostic `pkg/coordinator/storage.Service.Put`
that validates the MIME floor, encrypts (envelope v1) under a freshly-wrapped per-blob key
(keystore), imports deterministically (Kubo), and commits `data_encryption_keys` +
`blobs` + `blob_manifests` + `blob_blocks` (+ `collection_items`) atomically — returning
typed errors the handler maps to status codes.

**Status:** approved design (review incorporated) — implementation plan follows in
`docs/superpowers/plans/phase1/2026-05-29-phase1-m4-upload-pipeline.md`.

**Author:** Bug Plowman (operator), Claude (implementation partner).

---

## Purpose and scope

M4 is the fourth Phase 1 milestone. M1 delivered the schema + config foundation; M2
delivered the envelope codec, embedded hardened Kubo, the keystore, and the job queue; M3
made those reachable over HTTP for **reads**. M4 adds the **write path** that produces the
blobs M3 reads. It is the mirror image of M3: the same layering discipline (an HTTP-naïve
storage core returning typed errors, an HTTP layer that maps them), the same `nova_dev`
anonymous floor, the same dev-nginx integration harness.

M4 is deliberately **product-agnostic**. It builds the generic content-addressing write
path only. Every content-type-specific behavior — image decode, width/height, PDQ, format
conversion, `/i/*` routes, the `Product` interface and `RegisterProduct` — is M5.

### In scope

- `pkg/coordinator/storage.Service.Put`: the product-agnostic write transaction
  (validate → encrypt → import → manifest → commit → unpin-on-rollback). HTTP-naïve;
  returns typed errors and a `PutResult`. Reused by M5's derivative writes.
- `internal/upload`: hand-rolled **tus 1.0.0** (Creation + Core) session lifecycle backed
  by a new `upload_sessions` table (offset/metadata) + on-disk chunk file; plus a
  stale-session GC sweep.
- `internal/api/handlers/upload.go`: tus verbs (`POST`/`HEAD`/`PATCH`/`DELETE` +
  `POST .../finalize`); `internal/api/handlers/multipart.go`: `POST /api/v1/blobs`.
- Migration `0005_upload_sessions.sql`; sqlc write queries (blob/dek/manifest/blocks/
  collection-item inserts + session CRUD), committed generated code, CI drift gate.
- `internal/config` `Uploads` section: `max_upload_size_bytes`, `session_ttl_seconds`,
  `max_concurrent_assembly`, `tmp_dir`.
- A concurrency semaphore bounding in-memory assembly (V1-envelope RAM ceiling).
- The `derivative_prewarm` job-kind constant + a no-op stub handler.
- Unit tests (Put transaction + rollback/unpin, MIME floor, session offset/409/idempotency)
  and an nginx-fronted integration test (tus **and** multipart of JPEG/PNG/WebP → fetch
  byte-equal; manifest + blocks rows present).

### Out of scope (with the milestone that owns each)

- The `nova-image` product: `pkg/coordinator/product.Product` + `RegisterProduct`, govips
  transforms, PDQ perceptual hash, width/height, `image_metadata`, format conversion,
  `/api/v1/images`, `/i/*` — **M5**.
- Bearer auth issuance + middleware; real `owner_id` resolution — **M6** (M4 runs under the
  M3 `nova_dev` anonymous floor; uploads are anonymous, `owner_id` NULL).
- Collection-management API (`/api/v1/collections*`) — **M6** (M4's test seeds collections
  directly, as M3 did; uploads reference an existing `collection_id`).
- Authenticated metadata read/update + soft-delete (`GET/PATCH/DELETE /api/v1/blobs/{cid}`)
  — **M6** (metadata read/update) / **M9** (soft-delete + crypto-shred).
- `derivative_prewarm` **enqueue** site + handler body — **M5** (the post-commit hook lives
  in nova-image's `OnCommitted`; nothing in M4 produces derivatives to prewarm).
- A Kubo **orphan-pin reconciliation** loop (see "Crash gap" residual risk) — out of Phase 1.
- nginx TLS, dual origins, `proxy_cache`, request buffering tuning, `docker/Dockerfile` +
  a coordinator compose service — **M7/M11/M13**.
- `oapi-codegen` adoption — still deferred; M4's request bodies are tus headers + multipart
  + one small JSON result, hand-written more cheaply than wiring codegen (revisit when the
  `/api/v1` JSON surface grows in M6).

---

## Source of truth and required doc reconciliations

This spec is authoritative for M4 behavior. Two cross-document drifts surfaced during
design; both are resolved here and the edits are applied after this spec is reviewed
(same pattern M3 used for the `audit_log` line).

1. **nova-image AnalyzeUpload placement.** The Phase-1 design's milestone list
   (`…single-node-mvp-design.md:872-875`) puts "nova-image AnalyzeUpload (width/height/PDQ)"
   under M4, but the same document's M5 entry and `PRODUCT_MODULE_INTERFACE.md` assign the
   `Product` interface, govips, and PDQ to nova-image — and M3 already deferred
   `RegisterProduct` and the product to M5. **Resolution: M4 is product-agnostic; all
   nova-image analysis is M5.** Edit: amend the M4 milestone line to read "product-agnostic
   write path; nova-image AnalyzeUpload → M5."

2. **`/api/v1/images` placement.** The master plan's M4 deliverables
   (`…single-node-mvp.md:64`) list `/api/v1/images` alongside `/api/v1/blobs`. But
   `/api/v1/images` is image-product-specific by contract — it rejects non-image MIME types
   (`415`) and runs the perceptual-hash blocklist scan synchronously (`422`), both of which
   are nova-image's `AnalyzeUpload`. **Resolution: M4 ships `/api/v1/uploads` (tus) +
   `/api/v1/blobs` (generic multipart); `/api/v1/images` is M5.** Edit: move `/api/v1/images`
   to the M5 deliverable list.

3. **`AnalyzeUpload` in the canonical ordering (clarification, not a contradiction).**
   `PRODUCT_MODULE_INTERFACE.md` step 4 runs the product `AnalyzeUpload` hook before
   encryption. In M4 no product is registered, so the hook is a **no-op seam**: the pipeline
   validates the generic MIME floor, sets `blobs.product = 'raw'`, and proceeds straight to
   encrypt. M5 inserts nova-image at this seam.

The openapi upload endpoints all declare `security: [{ bearerAuth: [] }]`. Auth issuance is
M6; until then the write path runs under M3's `nova_dev` anonymous floor (a production build
still refuses `auth.anonymous: true`). No openapi edit is needed — the contract is correct;
M4 simply has no issuer to satisfy it yet, exactly as M3's read path had none.

---

## Preconditions from M2 / M3

M4 builds directly on these. If any diverges from its committed state, M4 estimates are
optimistic and the seam must be reconciled first.

| Dependency | Symbol / location | M4 use |
|---|---|---|
| Per-blob key wrap | `(*envelope.Keystore).Wrap(pbk) → (wrapped, mkvID)` | Wrap the fresh per-blob key under the active master version |
| Master-version bootstrap | `(*envelope.Keystore).Bootstrap(ctx)` (idempotent `master_key_versions` v1 insert) | Already satisfies "master-key versioning bootstrap"; M4 calls `Wrap` inside the write tx |
| Envelope v1 encrypt | `envelope.V1().Encrypt(plaintext, pbk) → env` | Produce the stored envelope bytes |
| Deterministic import | `ipfs.Backend.AddDeterministic(env) → AddResult{CID, EnvelopeSize, Codec, Blocks, MerkleRoot}` | Import + pin; supply manifest/blocks rows |
| Unpin | `ipfs.Backend.Unpin(cid)` | Best-effort rollback of the orphan import on tx failure |
| Read service | `pkg/coordinator/storage.Service` (`q`, `backend`, `ks`) | M4 extends it with `Put`; the round-trip test reads back via `Resolve`/`OpenBytes` |
| sqlc + committed gen | `internal/db/gen`, `internal/db/sqlc.yaml`, `make codegen-check` | M4 adds write queries to the same generated surface |
| Jobs | `jobs.Queue.Enqueue`, `(*WorkerPool).RegisterHandler` | `derivative_prewarm` stub kind |
| `nova_dev` floor | `internal/auth/anonymous_{dev,prod}.go` | Anonymous uploads in dev; refuse-to-start in prod |
| Hand-seed reference | `internal/blobfixture.Seed` | The non-transactional prototype M4 promotes to a real transaction |

`internal/blobfixture.Seed` already performs the exact insert set M4 needs, but
**non-transactionally and with raw pgx**. M4 promotes it to a single transaction over sqlc
queries with unpin-on-rollback; `blobfixture` remains the read-test seed helper (it may be
re-expressed on top of `Service.Put` once M4 lands, but that is not required by this spec).

---

## Architecture

```
internal/api/handlers/upload.go      ── tus verbs → internal/upload.Store
internal/api/handlers/multipart.go   ── POST /api/v1/blobs → storage.Service.Put
        │
        ▼
internal/upload/                     ── tus 1.0.0 (Creation+Core) session lifecycle
   store.go    Create/Offset/AppendChunk/Finalize/Abort/GC
   session.go  Session view; per-session in-process lock map (TryLock → 409)
   (chunks on disk under <tmp_dir>/<id>/data; metadata/offset in upload_sessions)
        │ Finalize reads the assembled file, calls ↓
        ▼
pkg/coordinator/storage/             ── WRITE CORE (product-agnostic, HTTP-naïve)
   put.go      Service.Put(ctx, r, declaredSize, PutContext) (*PutResult, error)
   types.go    PutContext, PutResult (+ existing read types)
   errors.go   + ErrUploadTooLarge / ErrMimeRejected / ErrCollectionNotFound /
               ErrServerBusy (existing read sentinels unchanged)
        │ uses
        ▼
internal/envelope (Wrap/Encrypt) · internal/ipfs (AddDeterministic/Unpin) · internal/db/gen
internal/db/migrations/0005_upload_sessions.sql
internal/jobs/kinds/derivative_prewarm.go   ── no-op stub handler (kind constant)
```

### Layering rationale (consistent with M3)

- **`storage.Service.Put` is HTTP-naïve and product-agnostic**, mirroring how `Resolve`/
  `OpenBytes` are. It takes an `io.Reader` + declared size + a `PutContext` (validated
  metadata) and returns domain errors; only `internal/api` knows status codes. This keeps
  the write core reusable by M5's derivative writes (the spec's `storage.PutDerivative`
  helper is `Put` with `parent_cid`/`derivative_*` set — M5 work).
- **tus protocol mechanics live in `internal/upload`, not the storage core.** Offsets,
  chunk files, `Tus-Resumable` headers, and the session row are protocol concerns; the
  storage core only sees the assembled plaintext at finalize. This is the same split that
  kept `internal/envelope` DB-naïve.
- **`pkg/coordinator` wiring.** `coordinator.New` constructs the `upload.Store` (given the
  pool, the `storage.Service`, and the `Uploads` config) and mounts the upload handlers;
  `Run` starts the stale-session GC ticker and stops it on shutdown. `Service` construction
  changes are additive (see "File structure").

### Service.Put signature

```go
// PutContext carries validated, product-agnostic write metadata.
type PutContext struct {
    MIME         string      // declared MIME; the sniff floor (below) must pass
    Product      string      // blob_product; M4 always "raw"
    CollectionID *uuid.UUID  // optional destination; resolves public_archival branch
    OwnerID      *uuid.UUID  // nil under the M4 anonymous dev floor
    SourceIP     netip.Addr  // zero value ⟹ not recorded (paranoid mode)
}

type PutResult struct {
    CID       string
    ByteSize  int64  // plaintext size
    MIME      string
    Product   string
    Encrypted bool
}

func (s *Service) Put(ctx context.Context, r io.Reader, declaredSize int64, pc PutContext) (*PutResult, error)
```

`Put` owns the in-memory window: it acquires the assembly semaphore, reads exactly
`declaredSize` bytes, runs the MIME floor, encrypts/imports, commits, and releases — so a
single knob bounds worst-case RAM regardless of which transport called it.

---

## Data model addition: `upload_sessions` (migration 0005)

No `upload_sessions` table exists in `DATA_MODEL.sql`; tus needs durable per-session offset
+ metadata (the chosen design keeps sessions SQL-observable and admin-GC-able). Bytes live
on disk; the row is the offset source of truth.

```sql
-- +goose Up
CREATE TYPE upload_session_state AS ENUM ('in_progress', 'finalized', 'aborted');

CREATE TABLE upload_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid REFERENCES users (id),                         -- NULL under the M4 anon floor; set in M6
    declared_length bigint NOT NULL CHECK (declared_length >= 0),       -- tus Upload-Length
    offset_bytes    bigint NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    mime_type       text,                                               -- declared; floor-validated at finalize
    product         blob_product NOT NULL DEFAULT 'raw',
    collection_id   uuid REFERENCES collections (id),
    state           upload_session_state NOT NULL DEFAULT 'in_progress',
    blob_cid        text,                                               -- set on finalize ⟹ idempotent re-finalize
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,                               -- created_at + uploads.session_ttl_seconds

    CONSTRAINT offset_within_length CHECK (offset_bytes <= declared_length)
);

CREATE INDEX upload_sessions_gc_idx ON upload_sessions (expires_at) WHERE state = 'in_progress';

CREATE TRIGGER upload_sessions_updated_at
    BEFORE UPDATE ON upload_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose Down
DROP TABLE upload_sessions;
DROP TYPE upload_session_state;
```

Design notes:

- **No `filename` column.** `blobs` deliberately stores no filename (DATA_MODEL.sql:281-299);
  filenames leak PII and are irrelevant to content addressing. tus clients still send
  `filename` in `Upload-Metadata`; M4 **accepts and discards** it rather than persisting it.
- **Plain table, not partitioned.** Sessions are short-lived and bounded by concurrent +
  recent uploads, and are GC'd — unlike `jobs`, partitioning buys nothing.
- **`expires_at` computed in the application** from `uploads.session_ttl_seconds` (so the TTL
  is one config knob, not split between SQL and config).

---

## Write pipeline (`storage.Service.Put`)

```
Put(ctx, r, declaredSize, pc):

0. sem.TryAcquire() — fail ⟹ ErrServerBusy (503). Held until return (defer release).
1. declaredSize > max_upload_size_bytes ⟹ ErrUploadTooLarge (413).   # defensive; handler also checks
2. buf := make([]byte, declaredSize); io.ReadFull(r, buf).
3. MIME floor (see "MIME validation"): reject ⟹ ErrMimeRejected (400). Decide stored mime_type.
4. Resolve write mode:
     pc.CollectionID set → load collection (visibility, public_archival)
        no row ⟹ ErrCollectionNotFound (404)
        public_archival = true → PLAINTEXT mode (no DEK; encryption_key_id NULL)
        else                   → ENCRYPTED mode
     pc.CollectionID nil → ENCRYPTED mode (blob has no membership ⟹ PRIVATE on read)
5. Build stored bytes:
     ENCRYPTED: pbk := CSPRNG(32); wrapped, mkvID := ks.Wrap(pbk); env := envelope.V1().Encrypt(buf, pbk)
     PLAINTEXT: env := buf
6. add := backend.AddDeterministic(ctx, env)   # imports + PINS (side effect, before the tx)
7. tx := pool.Begin(ctx); qtx := s.q.WithTx(tx):
     ENCRYPTED: INSERT data_encryption_keys(algorithm,'XChaCha20-Poly1305', wrapped, mkvID, 'active') → keyID
     INSERT blobs(cid, encryption_key_id=keyID|NULL, owner_id=pc.OwnerID, mime_type, byte_size=len(buf),
                  state='active', source_ip=pc.SourceIP|NULL, product=pc.Product, envelope_version=1)
                  ON CONFLICT (cid) DO NOTHING            # only collides for PLAINTEXT identical bytes
     INSERT blob_manifests(cid, hash_alg='sha2-256', codec=add.Codec, chunker='size-262144',
                           plaintext_size=len(buf), envelope_size=add.EnvelopeSize, block_count=len(add.Blocks),
                           merkle_root=add.MerkleRoot|NULL)  ON CONFLICT (cid) DO NOTHING
     INSERT blob_blocks(blob_cid, block_cid, block_index, block_size) for each add.Blocks  ON CONFLICT DO NOTHING
     if pc.CollectionID: INSERT collection_items(collection_id, blob_cid) ON CONFLICT DO NOTHING
     COMMIT
8. on tx error: backend.Unpin(ctx, add.CID) (best-effort; log on failure) → wrap as 500
9. return PutResult{CID, ByteSize=len(buf), MIME, Product, Encrypted}
```

### Atomicity boundary and the crash gap (residual risk)

The Kubo pin (step 6) happens **before** the Postgres commit (step 7), and the two stores
cannot be made atomic. Step 8's `Unpin` covers an ordinary transaction failure. It does
**not** cover a hard process death (OOM-kill, power loss) between the pin and the commit:
that leaves a CID pinned in Kubo with no `blobs` row.

- **Impact is bounded to local-disk waste.** An orphan has no `blobs` row, so it is
  unreadable through the API (read → 404) and carries no privacy or correctness consequence;
  it is storage leakage only.
- **No existing audit catches it.** The M8 `kubo_pin_present` audit verifies the *forward*
  direction (a `blobs`/`blob_blocks` row whose pin is present); it samples from the database,
  not from Kubo's pinset, so it never sees a pin that lacks a row.
- **The permanent fix** is a reconciliation sweep (Kubo pinset − `blobs.cid` − in-flight
  imports = orphans, unpin after a safety window). That is a background job explicitly
  **out of M4 scope**; M4 documents the gap and relies on best-effort `Unpin`. Recorded as a
  Phase-1 residual risk (paralleling `THREAT_MODEL.md`'s residual-risk entries).

### Deduplication nuance

Random-nonce v1 envelopes mean two uploads of identical plaintext produce **different**
envelope bytes and therefore **different** CIDs — so the **ENCRYPTED** path never collides
on the `blobs` primary key. Only the **PLAINTEXT** (`public_archival`) path can re-import
identical bytes to the same CID; the `ON CONFLICT (cid) DO NOTHING` clauses make a repeat
public-archival upload idempotent (re-pins harmlessly, attaches the collection membership,
returns the existing CID). `expected_cid`-style plaintext dedup was removed from the spec
(v2) and is not reintroduced here.

### Memory budget and the assembly semaphore

V1 is single-shot: `Put` holds the plaintext **and** the envelope in memory for the
encrypt+import window (~2× the object). The relevant bound on concurrent such windows is the
number of concurrent finalize/multipart calls — **not** `federation.max_pin_concurrency`
(a replication knob) — and M4 otherwise leaves that unbounded. `uploads.max_concurrent_assembly`
(default 8) is a buffered-channel semaphore (capacity = the limit; no new dependency)
acquired non-blockingly at the top of `Put`; saturation returns **503** `server_busy`. Worst-case assembly RAM ≈ `max_concurrent_assembly × max_upload_size_bytes × ~2`
≈ `8 × 100 MiB × 2` ≈ **1.6 GiB**, which fits a 4 GiB host. The semaphore is **not** held
during tus `PATCH` chunk appends (those stream to disk, no in-memory encrypt). Phase 2's
streaming-AEAD envelope removes the whole-object buffering and lifts the ceiling.

---

## tus protocol surface, multipart, and size limits

### tus 1.0.0 (Creation + Core), hand-rolled

| Verb | Endpoint | Behavior |
|---|---|---|
| `POST` | `/api/v1/uploads` | Require `Tus-Resumable: 1.0.0` + `Upload-Length`. Reject `Upload-Length > max_upload_size_bytes` → **413**. Parse `Upload-Metadata` (`mime_type`, `product`, `collection_id`; `filename` accepted+discarded). INSERT `upload_sessions` (`expires_at = now()+ttl`), `mkdir <tmp_dir>/<id>/`, create empty `data` file. **201** + `Location: /api/v1/uploads/<id>`, `Tus-Resumable`, `Upload-Offset: 0`. |
| `HEAD` | `/api/v1/uploads/{id}` | Load session (404 if absent/aborted). **200** + `Upload-Offset` (= `offset_bytes`), `Upload-Length`, `Tus-Resumable`, `Cache-Control: no-store`. |
| `PATCH` | `/api/v1/uploads/{id}` | Require `Content-Type: application/offset+octet-stream`, `Upload-Offset`. See concurrency below. Seek-write at the validated offset; `fsync`; optimistic offset commit. **204** + new `Upload-Offset`; offset mismatch → **409**; concurrent PATCH → **409**; would exceed `declared_length` → **400**. |
| `DELETE` | `/api/v1/uploads/{id}` | Mark `state='aborted'`, `rm -rf <tmp_dir>/<id>/`. **204** (404 if absent). |
| `POST` | `/api/v1/uploads/{id}/finalize` | If already `finalized` → return the stored `UploadResult` (idempotent). If `offset_bytes != declared_length` → **409**. Else open the chunk file and call `Service.Put`; on success set `state='finalized'`, `blob_cid`, `rm -rf` the dir, return **200** `UploadResult`. Map `Put` errors (below). |

### Concurrency safety on `PATCH` (in-process lock + optimistic commit)

A single coordinator serializes concurrent appends to one session with an **in-process
per-session `TryLock`** (a small `map[uuid]*sync.Mutex` with non-blocking acquire); a loser
returns **409** immediately. This is the same model as `tusd`'s default single-node
`MemoryLocker`. The handler then:

1. Loads the session; verifies `state='in_progress'` and `Upload-Offset == offset_bytes`
   (else 409).
2. Opens the chunk file, `Seek(offset_bytes)`, copies the body (capped at
   `declared_length - offset_bytes`), `fsync`. Seek-write (not append) makes an honest
   client retry idempotent.
3. Commits the offset **optimistically**:
   `UPDATE upload_sessions SET offset_bytes = $new WHERE id = $1 AND offset_bytes = $expected AND state = 'in_progress'`.
   `rows = 0` ⟹ a concurrent writer advanced it → **409**.

This keeps the DB `offset_bytes` authoritative and turns any lost race into a clean 409
**without holding a transaction/`FOR UPDATE` across the byte transfer** — which would pin one
of the pool's 16 connections to each in-flight (possibly slow, up to-100 MiB) `PATCH` and let
~16 slow uploaders starve all other DB work. The optimistic `WHERE offset_bytes = $expected`
is also the cross-process backstop for the Phase 2 multi-coordinator topology.

### Multipart fallback

`POST /api/v1/blobs` (`multipart/form-data`): fields `file` (required), `product` (default
`raw`), `collection_id`, `alt_text`/`caption` (accepted; image-only side metadata, ignored in
M4). Stream `file` to a temp path under `tmp_dir` (bounded by `max_upload_size_bytes`; over →
**413**), then call `Service.Put` with the part's size. **201** `UploadResult`. (M4 streams to
a temp file rather than buffering the multipart part in memory, so the assembly semaphore in
`Put` remains the single RAM bound.)

### `Content-Length` / size enforcement summary

`max_upload_size_bytes` is enforced at tus create (`Upload-Length`), defensively in `Put`,
and on the multipart stream; the `PATCH` per-chunk ceiling is `declared_length`.

---

## HTTP contract

### `UploadResult` (tus finalize, multipart)

Per `openapi.yaml` `UploadResult`: `{ cid, byte_size, mime_type, product, urls }` where for
M4's `product = 'raw'` blobs `urls.original = "/blob/{cid}"`, `urls.json =
"/blob/{cid}.json"`, and `presets` is omitted (no derivatives in M4). URLs are absolute,
built from the forwarded host (`X-Forwarded-Host`/`Host`), falling back to relative — the
same builder M3 uses for `Blob.urls`. Responses carry `X-Nova-Envelope-Version: 1` and
`X-Nova-Cid`.

### Error → status → openapi response

| `Put` / handler result | status | openapi response / `code` |
|---|---|---|
| created (multipart) | 201 | `UploadResult` |
| finalized (tus) | 200 | `UploadResult` |
| tus create / chunk accepted | 201 / 204 | headers only |
| bad/missing tus headers, malformed metadata, MIME floor reject | 400 | `BadRequest` / `bad_request`, `mime_rejected` |
| unknown / aborted session | 404 | `NotFound` / `not_found` |
| `ErrCollectionNotFound` | 404 | `NotFound` / `not_found` |
| offset mismatch / concurrent PATCH / finalize-incomplete | 409 | `Error` / `offset_conflict`, `upload_incomplete` |
| `ErrUploadTooLarge` | 413 | `Error` / `payload_too_large` |
| `ErrServerBusy` (assembly saturated) | 503 | `Error` / `server_busy` (+ `Retry-After`) |
| Kubo / wrap / encrypt / commit / failed-unpin | 500 | `Error` / `internal` |

All non-2xx bodies use the existing `Error` schema + `WriteError` helper (`request_id` from
middleware). The router mounts `/api/v1/uploads*` and `/api/v1/blobs` under the previously
404-reserved `/api/v1` subtree; the rest of `/api/v1/*` stays 404 until M6.

### Auth posture

All upload endpoints are `bearerAuth` in the contract, but M4 runs under the `nova_dev`
anonymous floor: a dev build serves them anonymously; a production build refuses to start
with `auth.anonymous: true` (M3 logic, unchanged). Anonymous ⟹ `owner_id = NULL`. `source_ip`
is recorded from the forwarded client address subject to `source_ip_retention_days`, and is
stored **NULL in paranoid mode** (data minimization).

---

## Config additions

```go
type Uploads struct {
    MaxUploadSizeBytes    int64  `yaml:"max_upload_size_bytes"`   // default 104857600 (100 MiB) — V1-envelope RAM ceiling
    SessionTTLSeconds     int    `yaml:"session_ttl_seconds"`     // default 86400 (24h)
    MaxConcurrentAssembly int    `yaml:"max_concurrent_assembly"` // default 8 — bounds in-memory encrypt windows
    TmpDir                string `yaml:"tmp_dir"`                 // default the nova-tmp-uploads mount
}
```

Added to `Config` as `Uploads Uploads \`yaml:"uploads"\``, with loader defaults in
`operator_yaml.go`. The 100 MiB default is documented as an artificial Phase-1 ceiling tied
to V1 whole-object encryption, liftable by the Phase-2 streaming-AEAD envelope. Paranoid mode
does not change the size ceiling but suppresses `source_ip` persistence (above).

---

## `derivative_prewarm` job-kind stub

M4 adds the kind constant and a no-op handler:

```go
// internal/jobs/kinds/derivative_prewarm.go
const KindDerivativePrewarm = "derivative_prewarm"

// DerivativePrewarmStub is a no-op until M5 wires nova-image's OnCommitted.
func DerivativePrewarmStub(ctx context.Context, payload []byte) error { return nil }
```

This satisfies the master plan's "derivative_prewarm job kind (stub Phase 1)". The
**enqueue** site and the real handler body land in M5 alongside nova-image's `OnCommitted`;
nothing in M4 produces derivatives, so the worker pool need not run in M4 (its startup is M5/
M8 work). A unit test asserts the stub returns nil.

---

## Stale-session GC

Finalize and abort delete their own `<tmp_dir>/<id>/` synchronously. Abandoned `in_progress`
sessions (client vanished mid-upload) are reclaimed by a lightweight ticker started in
`coordinator.Run` (default hourly), independent of the (M8) audit scheduler:

```
tick:
  ids := SELECT id FROM upload_sessions WHERE state='in_progress' AND expires_at < now()
  for each id:  rm -rf <tmp_dir>/<id>/            # filesystem first
  DELETE FROM upload_sessions WHERE id = ANY(ids) AND state='in_progress' AND expires_at < now()
  log.Info("upload gc", "deleted", len(ids))
```

**Filesystem cleanup precedes the row delete** so a crash mid-sweep leaves the row (retried
next tick) and never an orphaned directory — the same ordering discipline as the write
pipeline's unpin reasoning. The ticker stops on `Shutdown`.

---

## MIME validation (generic security floor)

`Put` reads the first 512 bytes and calls `http.DetectContentType` (stdlib; no new dep). The
rule (verified against the M4 exit formats — JPEG/PNG/WebP all sniff to the correct
`image/*`; AVIF sniffs to `application/octet-stream`):

- declared MIME empty → store the detected type;
- detected `application/octet-stream` (sniffer can't identify) → **accept** the declared type
  (so formats the sniffer doesn't know, e.g. AVIF, are not false-rejected);
- otherwise → **400** when the detected **top-level** type contradicts the declared one
  (e.g. declared `image/jpeg`, bytes are a script/executable → detected `text/*` ≠ `image/*`).

This is a cheap floor that blocks the XSS-relevant `text/*`-as-image confusion. It does
**not** prove the bytes are a *valid instance* of the declared subtype — strict per-format
decode validation is nova-image's `AnalyzeUpload` in M5. M4 stores the declared (floor-passed)
MIME on the blob.

---

## Startup validation

Unchanged from M3. `cmd/coordinator` still runs `ipfs.ValidateConfig`, `Keystore.Bootstrap`,
the non-root check, and the `nova_dev` anonymous-policy gate before serving. M4 adds creation
of `uploads.tmp_dir` (mode `0700`) at startup and refuses to start if it is not writable —
the write path's first debugging input, consistent with the refuse-to-start ethos.

---

## Testing strategy

### Unit

- **`storage.Service.Put`** against a testcontainer Postgres (`internal/dbtest`) + migrations +
  a **fake `ipfs.Backend`**: encrypted and `public_archival` modes; `collection_id` present/
  absent/unknown; `byte_size`/manifest/block rows correct; `product='raw'`. **Rollback +
  unpin:** inject a failing tx (e.g., a constraint violation or a wrapped commit error) and a
  backend that records `Unpin`; assert `Unpin(cid)` is called exactly once and the error maps
  to 500. **Semaphore:** saturate `max_concurrent_assembly` and assert `ErrServerBusy`.
- **MIME floor** (table-driven): the rule above, including JPEG/PNG/WebP accept, AVIF-as-
  octet-stream accept, and `image/*`-declared-but-`text/*`-detected reject.
- **`internal/upload.Store`** against testcontainer Postgres + a temp dir: create → offset →
  append (correct offset) → finalize round-trip; offset-mismatch → 409; concurrent `PATCH`
  (two goroutines, same offset) → exactly one 204 and one 409, file uncorrupted; re-finalize
  idempotent (same `UploadResult`); abort removes the dir; GC removes an expired `in_progress`
  session's row **and** dir, leaving `finalized` sessions untouched.

### Integration (`internal/integration/m4_upload_test.go`)

Testcontainer Postgres + migrations + a real `EmbeddedBackend` (offline) + `Keystore`, the
coordinator in-process, fronted by the **dev nginx** proxy (the M3 harness, extended to pass
`/api/v1/uploads*` and `/api/v1/blobs`). A seed helper creates a public collection by hand
(as M3 seeded data). For **each of JPEG, PNG, WebP**, via **both** transports:

1. **tus:** `POST /api/v1/uploads` → `PATCH` the bytes (in ≥2 chunks for at least one case to
   exercise resumption) → `POST .../finalize` → `UploadResult`.
2. **multipart:** `POST /api/v1/blobs` with the file + `collection_id` → `UploadResult`.

Then assert: `GET /blob/{cid}` through nginx → **200** and **byte-equal** to the original;
`blob_manifests` and `blob_blocks` rows exist with the right counts; the blob's collection
membership makes it anonymously readable. Negative cases: oversize create → **413**; offset
mismatch → **409**; finalize before complete → **409**; bad-MIME (declare `image/jpeg`, send a
script) → **400**. A representative few-MiB upload exercises the whole-object assembly path
(symmetric to M3's read-size budget).

**Fallback** (same as M3): if the nginx→in-process wiring is flaky in CI, the automated test
targets the coordinator directly and nginx is exercised by a `make smoke`-style check; the
plan records this explicitly.

### CI gates

`make codegen-check` continues to gate sqlc drift (now covering the write + session queries).
Existing `test`/`vet`/`lint`/`schema-drift` gates run unchanged.

---

## Security and privacy considerations

- **Anonymous writes are dev-only.** Uploads are reachable without a bearer only under the
  `nova_dev` build; a production build refuses to start with `auth.anonymous: true`. Real
  ownership + authorization arrive in M6. nginx rate-limiting (M3 middleware + the Phase-1
  nginx zones later) bounds anonymous abuse in the interim.
- **Data minimization.** No filename is persisted (sessions or blobs). `source_ip` is bounded
  by `source_ip_retention_days` and suppressed entirely in paranoid mode.
- **MIME floor** blocks `text/*`-as-image content-type confusion (a stored-XSS vector when
  bytes are later served); it is a floor, not full validation (M5).
- **No new Kubo exposure.** The embedded Kubo API/Gateway stay loopback-only per M2
  `ValidateConfig`; the write path adds only local `AddDeterministic`/`Unpin` calls.
- **Operator-visible plaintext is intended** (Tier 1). `Put` decrypts nothing; it encrypts on
  ingest. The operator's master key wraps every per-blob key.

---

## Risks and notes

1. **Crash gap → orphaned pins** (above): a hard crash between Kubo pin and DB commit leaks a
   pinned, unreadable CID. Bounded to disk waste; best-effort `Unpin` only; reconciliation
   sweep is post-M4. Recorded as a Phase-1 residual risk.
2. **Whole-object assembly RAM**: bounded by `max_concurrent_assembly × max_upload_size_bytes
   × ~2`; documented ceiling, resolved structurally by Phase-2 streaming AEAD.
3. **`Service` construction change**: adding the pool + semaphore to `Service` (for `Put`'s
   transactions and RAM bound) changes `NewService`'s signature — an internal, additive change
   (M3 call sites in `cmd/coordinator` + tests update in the same milestone).
4. **tus extensions**: M4 implements Creation + Core only (sufficient for Uppy in M12).
   Concatenation, Checksum, and Expiration-header advertisement are not implemented; the GC
   ticker enforces expiration server-side without advertising the extension.
5. **Spec drift**: the two reconciliations above are applied to the Phase-1 design + master
   plan; further gaps discovered in implementation follow the project's v3.x amendment pattern.

---

## File structure

### Created in M4

| Path | Purpose |
|---|---|
| `internal/db/migrations/0005_upload_sessions.sql` | `upload_sessions` table + enum + trigger |
| `internal/db/queries/uploads.sql` | session CRUD (create, get, append-offset, finalize, list-expired, delete) |
| `internal/db/queries/writes.sql` | blob/dek/manifest/block/collection-item inserts (tx-composed) |
| `internal/upload/store.go` | tus session lifecycle (Create/Offset/AppendChunk/Finalize/Abort/GC) |
| `internal/upload/session.go` | `Session` view + in-process per-session lock map |
| `internal/upload/store_test.go` | session unit tests (pg + temp dir) |
| `pkg/coordinator/storage/put.go` | `Service.Put` write transaction |
| `pkg/coordinator/storage/put_test.go` | Put tests (pg + fake backend; rollback/unpin; semaphore) |
| `internal/api/handlers/upload.go` | tus verbs |
| `internal/api/handlers/multipart.go` | `POST /api/v1/blobs` |
| `internal/api/handlers/upload_test.go` | handler tests (httptest + fake store/service) |
| `internal/jobs/kinds/derivative_prewarm.go` | kind constant + no-op stub |
| `internal/integration/m4_upload_test.go` | nginx-fronted tus + multipart round-trip |

### Modified in M4

| Path | Why |
|---|---|
| `pkg/coordinator/storage/blob.go` | `Service` gains `pool` + assembly semaphore; `NewService` signature (additive) |
| `pkg/coordinator/storage/types.go` | `PutContext`, `PutResult` |
| `pkg/coordinator/storage/errors.go` | `ErrUploadTooLarge`, `ErrMimeRejected`, `ErrCollectionNotFound`, `ErrServerBusy` |
| `pkg/coordinator/coordinator.go` | construct `upload.Store`; mount upload handlers; start/stop GC ticker |
| `internal/api/server.go` | mount `/api/v1/uploads*` + `/api/v1/blobs` |
| `cmd/coordinator/main.go` | load `Uploads` config; create/validate `tmp_dir` |
| `internal/config/types.go` + `operator_yaml.go` | `Uploads` section + defaults |
| `internal/db/gen/*` | regenerated for the new queries (committed) |
| `docker/nginx/nova.dev.conf` | pass `/api/v1/uploads*` + `/api/v1/blobs` to the coordinator |
| `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md` | reconcile M4 milestone line (nova-image AnalyzeUpload → M5) |
| `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md` | mark M4 in progress; move `/api/v1/images` to M5; link M4 plan |

---

## Cross-references

- `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md` — Phase 1 architecture
  (upload pipeline §, job lifecycle §, container topology § for the `nova-tmp-uploads` volume).
- `docs/superpowers/specs/phase1/2026-05-28-phase1-m3-storage-read-api-design.md` — the read path M4
  feeds and the layering/`nova_dev`/dev-nginx patterns M4 mirrors.
- `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md` — milestone table + M4 summary.
- `docs/specs/openapi.yaml` — `/api/v1/uploads*`, `/api/v1/blobs`, `UploadResult`, `Error`.
- `docs/specs/DATA_MODEL.sql` — `blobs`, `data_encryption_keys`, `blob_manifests`,
  `blob_blocks`, `collections`, `collection_items` (write targets); `set_updated_at()`.
- `docs/specs/PRODUCT_MODULE_INTERFACE.md` — canonical upload ordering; the `AnalyzeUpload`
  seam M4 leaves as a no-op and M5 fills.
- `docs/specs/IPFS_IMPORT_RULES.md` — deterministic import parameters `AddDeterministic` honors.
- `docs/specs/ENCRYPTION_ENVELOPE.md` — v1 single-shot encrypt + master-key wrap.
- `docs/specs/ARCHITECTURE_DECISIONS.md` — Tier 1 (operator-visible plaintext; no telemetry).
