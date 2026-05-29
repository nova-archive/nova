# Phase 1 M4 — Upload Pipeline (Write Path) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A curl-driven `tus` resumable upload **or** a `multipart/form-data` upload encrypts, imports deterministically to Kubo, commits the blob/manifest/block/key rows in one transaction (unpinning on rollback), and returns a CID fetchable via M3's read path.

**Architecture:** `internal/api` upload handlers drive a hand-rolled tus 1.0.0 session store (`internal/upload`, backed by a new `upload_sessions` table + on-disk chunk file) and, at finalize/multipart, call a product-agnostic `pkg/coordinator/storage.Service.Put` (validate MIME floor → encrypt → import → commit → unpin-on-rollback). Concurrency is bounded by an in-process per-session lock + optimistic offset commit, and assembly RAM by a buffered-channel semaphore. See `docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md`.

**Tech Stack:** Go 1.22, Postgres 16 (pgx/v5 + sqlc), Kubo (embedded), chi router, goose migrations, testcontainers-go, stdlib `net/http` (tus + multipart + `DetectContentType`). No new third-party dependencies.

---

## Files

**Created:**

| Path | Responsibility |
|---|---|
| `internal/db/migrations/0005_upload_sessions.sql` | `upload_sessions` table + enum + trigger |
| `internal/db/queries/writes.sql` | blob/dek/manifest/block/collection-item inserts + collection-for-write lookup |
| `internal/db/queries/uploads.sql` | session CRUD (create/get/advance-offset/finalize/abort/list-expired/delete) |
| `pkg/coordinator/storage/put.go` | `Service.Put` write transaction + `validateMIME` |
| `pkg/coordinator/storage/put_test.go` | Put tests (pg + fake backend; encrypted/public_archival; rollback-unpin; semaphore; MIME) |
| `internal/upload/store.go` | tus session lifecycle (Create/Offset/AppendChunk/Finalize/Abort/GC) |
| `internal/upload/locks.go` | in-process per-session lock map |
| `internal/upload/store_test.go` | session unit tests (pg + temp dir + fake committer) |
| `internal/api/handlers/upload.go` | tus verbs + multipart handler |
| `internal/api/handlers/upload_test.go` | handler tests (httptest + fake store/committer) |
| `internal/jobs/kinds/derivative_prewarm.go` | `derivative_prewarm` kind constant + no-op stub |
| `internal/jobs/kinds/derivative_prewarm_test.go` | stub returns nil |
| `internal/integration/m4_upload_test.go` | nginx-fronted tus + multipart round-trip |

**Modified:**

| Path | Change |
|---|---|
| `cmd/migrate/main_test.go` | add `upload_sessions` to the expected-tables assertion |
| `internal/config/types.go` | `Uploads` section + default constants |
| `internal/config/operator_yaml.go` | apply `Uploads` defaults |
| `internal/config/operator_yaml_test.go` | assert defaults applied |
| `pkg/coordinator/storage/types.go` | `PutContext`, `PutResult` |
| `pkg/coordinator/storage/errors.go` | `ErrUploadTooLarge`, `ErrMimeRejected`, `ErrCollectionNotFound`, `ErrServerBusy` |
| `pkg/coordinator/storage/blob.go` | `Service` gains `pool` + assembly semaphore + max size; `NewService` variadic options |
| `internal/api/server.go` | mount `/api/v1/uploads*` + `/api/v1/blobs` when `Upload != nil` |
| `pkg/coordinator/coordinator.go` | construct upload store + handler; start GC ticker in `Run` |
| `cmd/coordinator/main.go` | upload env knobs; create `tmp_dir` |
| `docker/nginx/nova.dev.conf` | pass `/api/v1/uploads` + `/api/v1/blobs` |
| `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` | reconcile M4 line (nova-image AnalyzeUpload → M5) |
| `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md` | mark M4 in progress; `/api/v1/images` → M5; link M4 plan |
| `internal/db/gen/*` | regenerated (committed) |

---

## Task 1: Migration 0005 — `upload_sessions`

**Files:** Create `internal/db/migrations/0005_upload_sessions.sql`; Modify `cmd/migrate/main_test.go`.

- [ ] **Step 1.1: Create the migration**

```sql
-- +goose Up
-- +goose StatementBegin
-- Migration 0005: tus resumable-upload session table.
-- See docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md
-- § "Data model addition". Short-lived, GC'd, not partitioned. No filename
-- column (data minimization; blobs stores none either).

CREATE TYPE upload_session_state AS ENUM ('in_progress', 'finalized', 'aborted');

CREATE TABLE upload_sessions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid REFERENCES users (id),
    declared_length bigint NOT NULL CHECK (declared_length >= 0),
    offset_bytes    bigint NOT NULL DEFAULT 0 CHECK (offset_bytes >= 0),
    mime_type       text,
    product         blob_product NOT NULL DEFAULT 'raw',
    collection_id   uuid REFERENCES collections (id),
    state           upload_session_state NOT NULL DEFAULT 'in_progress',
    blob_cid        text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,

    CONSTRAINT offset_within_length CHECK (offset_bytes <= declared_length)
);

CREATE INDEX upload_sessions_gc_idx
    ON upload_sessions (expires_at)
    WHERE state = 'in_progress';

CREATE TRIGGER upload_sessions_updated_at
    BEFORE UPDATE ON upload_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE upload_sessions;
DROP TYPE upload_session_state;
-- +goose StatementEnd
```

- [ ] **Step 1.2: Add `upload_sessions` to the migrate integration test**

In `cmd/migrate/main_test.go`, add `"upload_sessions"` to the `expectedTables` slice (after `"jobs"`).

- [ ] **Step 1.3: Run the migrate test**

Run: `go test ./cmd/migrate/... -run Integration -v`
Expected: PASS (table present after `migrate up`).

- [ ] **Step 1.4: Commit**

```bash
git add internal/db/migrations/0005_upload_sessions.sql cmd/migrate/main_test.go
git commit -s -m "feat(db): migration 0005 — upload_sessions table for tus" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Config `Uploads` section + defaults

**Files:** Modify `internal/config/types.go`, `internal/config/operator_yaml.go`, `internal/config/operator_yaml_test.go`.

- [ ] **Step 2.1: Add the `Uploads` struct + defaults to `types.go`**

Add to `Config` (after the `Coordinator` field):

```go
	Uploads Uploads `yaml:"uploads"`
```

Append these types + default constants:

```go
// Upload-pipeline defaults (M4). The size ceiling is an artificial Phase-1
// limit tied to V1 whole-object encryption; Phase-2 streaming AEAD lifts it.
const (
	DefaultMaxUploadSizeBytes    int64 = 104857600 // 100 MiB
	DefaultUploadSessionTTLSecs        = 86400      // 24h
	DefaultMaxConcurrentAssembly       = 8
)

type Uploads struct {
	MaxUploadSizeBytes    int64  `yaml:"max_upload_size_bytes"`
	SessionTTLSeconds     int    `yaml:"session_ttl_seconds"`
	MaxConcurrentAssembly int    `yaml:"max_concurrent_assembly"`
	TmpDir                string `yaml:"tmp_dir"`
}
```

- [ ] **Step 2.2: Apply defaults in the loader**

In `internal/config/operator_yaml.go`, inside `LoadFromBytes`, after `ApplyParanoid(&cfg)` and before `return`, add:

```go
	applyUploadDefaults(&cfg)
```

Add the function at the end of the file:

```go
func applyUploadDefaults(cfg *Config) {
	if cfg.Uploads.MaxUploadSizeBytes <= 0 {
		cfg.Uploads.MaxUploadSizeBytes = DefaultMaxUploadSizeBytes
	}
	if cfg.Uploads.SessionTTLSeconds <= 0 {
		cfg.Uploads.SessionTTLSeconds = DefaultUploadSessionTTLSecs
	}
	if cfg.Uploads.MaxConcurrentAssembly <= 0 {
		cfg.Uploads.MaxConcurrentAssembly = DefaultMaxConcurrentAssembly
	}
}
```

- [ ] **Step 2.3: Add a defaults assertion to the loader test**

In `internal/config/operator_yaml_test.go`, inside `TestLoadMinimalOperatorYAML`, add:

```go
	require.Equal(t, config.DefaultMaxUploadSizeBytes, cfg.Uploads.MaxUploadSizeBytes)
	require.Equal(t, 86400, cfg.Uploads.SessionTTLSeconds)
	require.Equal(t, 8, cfg.Uploads.MaxConcurrentAssembly)
```

- [ ] **Step 2.4: Run config tests**

Run: `go test ./internal/config/... -v`
Expected: PASS.

- [ ] **Step 2.5: Commit**

```bash
git add internal/config/
git commit -s -m "feat(config): uploads section (size/ttl/assembly) with defaults" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: sqlc write + session queries

**Files:** Create `internal/db/queries/writes.sql`, `internal/db/queries/uploads.sql`; regenerate `internal/db/gen/*`.

- [ ] **Step 3.1: Create `internal/db/queries/writes.sql`**

```sql
-- name: GetCollectionForWrite :one
SELECT public_archival, visibility::text AS visibility
FROM collections
WHERE id = $1;

-- name: InsertDEK :one
INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
VALUES ($1, $2, $3, 'active')
RETURNING id;

-- name: InsertBlob :exec
INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, source_ip, product, state, envelope_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'active', 1)
ON CONFLICT (cid) DO NOTHING;

-- name: InsertManifest :exec
INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count, merkle_root)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (cid) DO NOTHING;

-- name: InsertBlock :exec
INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
VALUES ($1, $2, $3, $4)
ON CONFLICT (blob_cid, block_index) DO NOTHING;

-- name: InsertCollectionItem :exec
INSERT INTO collection_items (collection_id, blob_cid)
VALUES ($1, $2)
ON CONFLICT (collection_id, blob_cid) DO NOTHING;
```

- [ ] **Step 3.2: Create `internal/db/queries/uploads.sql`**

```sql
-- name: CreateUploadSession :one
INSERT INTO upload_sessions (owner_id, declared_length, mime_type, product, collection_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- name: GetUploadSession :one
SELECT id, owner_id, declared_length, offset_bytes, mime_type,
       product::text AS product, collection_id, state::text AS state, blob_cid
FROM upload_sessions
WHERE id = $1;

-- name: AdvanceUploadOffset :execrows
UPDATE upload_sessions
SET offset_bytes = $2
WHERE id = $1 AND offset_bytes = $3 AND state = 'in_progress';

-- name: FinalizeUploadSession :exec
UPDATE upload_sessions
SET state = 'finalized', blob_cid = $2
WHERE id = $1;

-- name: AbortUploadSession :exec
UPDATE upload_sessions
SET state = 'aborted'
WHERE id = $1;

-- name: ListExpiredUploadSessions :many
SELECT id
FROM upload_sessions
WHERE state = 'in_progress' AND expires_at < now();

-- name: DeleteUploadSession :exec
DELETE FROM upload_sessions
WHERE id = $1 AND state = 'in_progress';
```

- [ ] **Step 3.3: Regenerate sqlc + verify drift gate**

Run: `make sqlc-generate && make codegen-check`
Expected: `internal/db/gen` updated; `codegen-check` passes (no diff after regenerate). New methods appear in `gen.Querier`: `GetCollectionForWrite`, `InsertDEK`, `InsertBlob`, `InsertManifest`, `InsertBlock`, `InsertCollectionItem`, `CreateUploadSession`, `GetUploadSession`, `AdvanceUploadOffset`, `FinalizeUploadSession`, `AbortUploadSession`, `ListExpiredUploadSessions`, `DeleteUploadSession`.

- [ ] **Step 3.4: Build to confirm generated code compiles**

Run: `go build ./internal/db/...`
Expected: success.

- [ ] **Step 3.5: Commit**

```bash
git add internal/db/queries/ internal/db/gen/
git commit -s -m "feat(db): sqlc write + upload-session queries (committed gen)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: storage errors + write types

**Files:** Modify `pkg/coordinator/storage/errors.go`, `pkg/coordinator/storage/types.go`.

- [ ] **Step 4.1: Add write sentinels to `errors.go`**

Append:

```go
// Write-path domain errors (M4). The api layer maps these to status codes.
var (
	ErrUploadTooLarge     = errors.New("storage: upload exceeds max size")
	ErrMimeRejected       = errors.New("storage: declared mime contradicts content")
	ErrCollectionNotFound = errors.New("storage: collection not found")
	ErrServerBusy         = errors.New("storage: assembly capacity saturated")
)
```

(If `errors` is not already imported in `errors.go`, add it.)

- [ ] **Step 4.2: Add `PutContext`/`PutResult` to `types.go`**

```go
// PutContext carries validated, product-agnostic write metadata.
type PutContext struct {
	MIME         string
	Product      string // blob_product; M4 always "raw"
	CollectionID *uuid.UUID
	OwnerID      *uuid.UUID
	SourceIP     netip.Addr // zero value ⟹ not recorded
}

// PutResult reports the committed blob.
type PutResult struct {
	CID       string
	ByteSize  int64
	MIME      string
	Product   string
	Encrypted bool
}
```

Ensure `types.go` imports `"net/netip"` and `"github.com/google/uuid"`.

- [ ] **Step 4.3: Build**

Run: `go build ./pkg/coordinator/storage/...`
Expected: success (unused types are fine; Put lands next task).

- [ ] **Step 4.4: Commit**

```bash
git add pkg/coordinator/storage/errors.go pkg/coordinator/storage/types.go
git commit -s -m "feat(storage): write-path domain errors + PutContext/PutResult" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: MIME validation floor

**Files:** part of `pkg/coordinator/storage/put.go` (function only) + `put_test.go` (test only). This task writes just `validateMIME` and its test; `Service.Put` is Task 6.

- [ ] **Step 5.1: Write the failing test (`put_test.go`)**

```go
package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateMIME(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	webp := append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("WEBPVP8 ")...)
	script := []byte("#!/bin/sh\necho hi\n")

	cases := []struct {
		name     string
		declared string
		body     []byte
		want     string
		wantErr  bool
	}{
		{"jpeg ok", "image/jpeg", jpeg, "image/jpeg", false},
		{"png ok", "image/png", png, "image/png", false},
		{"webp ok", "image/webp", webp, "image/webp", false},
		{"empty declared uses detected", "", png, "image/png", false},
		{"unknown sniff accepts declared", "image/avif", []byte{0, 0, 0, 0x1c}, "image/avif", false},
		{"contradiction rejected", "image/jpeg", script, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateMIME(tc.declared, tc.body)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMimeRejected)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
```

- [ ] **Step 5.2: Run, verify it fails**

Run: `go test ./pkg/coordinator/storage/ -run TestValidateMIME`
Expected: FAIL (`validateMIME` undefined).

- [ ] **Step 5.3: Implement `validateMIME` (start `put.go`)**

Create `pkg/coordinator/storage/put.go` with the package clause, imports, and this function (Put added next task):

```go
package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
)

// validateMIME is a cheap generic content floor. It blocks the XSS-relevant
// case of a text/script body declared as an image, without rejecting formats
// the stdlib sniffer doesn't recognize (e.g. AVIF → octet-stream). It does NOT
// prove the bytes are a valid instance of the declared type — that is the
// product layer's (M5) decode validation.
func validateMIME(declared string, head []byte) (string, error) {
	detected := http.DetectContentType(head) // reads up to first 512 bytes
	if declared == "" {
		return detected, nil
	}
	if detected == "application/octet-stream" {
		return declared, nil // sniffer can't identify; trust the declaration
	}
	if topLevel(detected) != topLevel(declared) {
		return "", fmt.Errorf("%w: declared %q, detected %q", ErrMimeRejected, declared, detected)
	}
	return declared, nil
}

func topLevel(mime string) string {
	if i := strings.IndexByte(mime, '/'); i >= 0 {
		return mime[:i]
	}
	return mime
}
```

- [ ] **Step 5.4: Run, verify it passes**

Run: `go test ./pkg/coordinator/storage/ -run TestValidateMIME -v`
Expected: PASS.

- [ ] **Step 5.5: Commit**

```bash
git add pkg/coordinator/storage/put.go pkg/coordinator/storage/put_test.go
git commit -s -m "feat(storage): generic MIME content floor (DetectContentType)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: `Service.Put` write transaction

**Files:** Modify `pkg/coordinator/storage/blob.go` (Service struct + `NewService` options); add `Put`/`commit` to `pkg/coordinator/storage/put.go`; extend `put_test.go`.

- [ ] **Step 6.1: Extend the `Service` struct + `NewService` with options (`blob.go`)**

Replace the `Service` struct and `NewService` with:

```go
// Service is the storage core (read + write). Safe for concurrent use.
type Service struct {
	q             *gen.Queries
	pool          *pgxpool.Pool
	backend       ipfs.Backend
	ks            *envelope.Keystore
	maxUploadSize int64
	assembly      chan struct{} // buffered semaphore bounding in-memory assembly
}

// Option configures a Service. Existing read-only callers pass none.
type Option func(*svcOpts)

type svcOpts struct {
	maxUploadSize int64
	assemblySize  int
}

// WithWriteLimits sets the upload size ceiling and the max concurrent
// in-memory assembly operations (V1-envelope RAM bound).
func WithWriteLimits(maxUploadSize int64, maxConcurrentAssembly int) Option {
	return func(o *svcOpts) {
		if maxUploadSize > 0 {
			o.maxUploadSize = maxUploadSize
		}
		if maxConcurrentAssembly > 0 {
			o.assemblySize = maxConcurrentAssembly
		}
	}
}

// NewService builds a storage service over the given pool, IPFS backend, and
// keystore. backend and ks may be nil in tests that exercise Resolve only.
// Write limits default to 100 MiB / 8 concurrent assemblies; override via
// WithWriteLimits.
func NewService(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, opts ...Option) *Service {
	o := svcOpts{maxUploadSize: 104857600, assemblySize: 8}
	for _, fn := range opts {
		fn(&o)
	}
	return &Service{
		q:             gen.New(pool),
		pool:          pool,
		backend:       backend,
		ks:            ks,
		maxUploadSize: o.maxUploadSize,
		assembly:      make(chan struct{}, o.assemblySize),
	}
}
```

(Existing `NewService(pool, backend, ks)` call sites still compile — the variadic is empty.)

- [ ] **Step 6.2: Write the failing Put tests (extend `put_test.go`)**

Add (these are integration-style; they use a testcontainer pool + a fake backend):

```go
import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

// fakeBackend records pins/unpins and stores bytes by CID.
type fakeBackend struct {
	store     map[string][]byte
	unpinned  []string
	failAfter bool // when true, AddDeterministic succeeds but commit will fail
}

func newFakeBackend() *fakeBackend { return &fakeBackend{store: map[string][]byte{}} }

func (f *fakeBackend) AddDeterministic(ctx context.Context, env []byte) (ipfs.AddResult, error) {
	// Derive a deterministic CIDv1 raw from the bytes for the test.
	c, err := cid.V1Builder{Codec: cid.Raw, MhType: 0x12}.Sum(env)
	if err != nil {
		return ipfs.AddResult{}, err
	}
	f.store[c.String()] = append([]byte(nil), env...)
	return ipfs.AddResult{
		CID: c, EnvelopeSize: int64(len(env)), Codec: "raw",
		Blocks: []ipfs.Block{{CID: c, Index: 0, Size: len(env)}}, MerkleRoot: c,
	}, nil
}
func (f *fakeBackend) Get(ctx context.Context, c cid.Cid) (interface{ Read([]byte) (int, error) }, error) {
	return nil, errors.New("unused")
}
func (f *fakeBackend) Unpin(ctx context.Context, c cid.Cid) error {
	f.unpinned = append(f.unpinned, c.String())
	return nil
}

// NOTE: implement the remaining ipfs.Backend methods as no-ops in the real
// test file (Has/Pin/BlockstoreHas/BlockGet/Get/Close) — see Task 6.3 for the
// full fake. The snippet above shows the relevant behavior.

func seedCollection(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, publicArchival bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	var owner, col uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@put.test").Scan(&owner))
	vis := "private"
	if publicArchival {
		vis = "public"
	}
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'c','c',$2,$3) RETURNING id`, owner, vis, publicArchival).Scan(&col))
	return owner, col
}
```

(The exact fake-backend boilerplate is completed in Step 6.3; keep the test file compiling by writing the full fake there.)

- [ ] **Step 6.3: Implement `Put` + `commit` (append to `put.go`)**

```go
// Put encrypts (or, for public_archival collections, stores plaintext),
// imports deterministically to Kubo, and commits the blob + manifest + blocks
// (+ DEK, + collection membership) in one transaction. On any commit failure
// it best-effort unpins the orphaned Kubo import. The Kubo pin precedes the
// DB commit and the two cannot be made atomic: a hard crash in between leaks a
// pinned, unreadable CID (documented residual risk; reconciliation sweep is
// out of M4 scope).
//
// Put owns the in-memory window (a buffered-channel semaphore bounds concurrent
// assemblies) and reads exactly declaredSize bytes from r.
func (s *Service) Put(ctx context.Context, r io.Reader, declaredSize int64, pc PutContext) (*PutResult, error) {
	if declaredSize > s.maxUploadSize {
		return nil, ErrUploadTooLarge
	}
	select {
	case s.assembly <- struct{}{}:
		defer func() { <-s.assembly }()
	default:
		return nil, ErrServerBusy
	}

	buf := make([]byte, declaredSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("storage: read upload body: %w", err)
	}

	mime, err := validateMIME(pc.MIME, buf)
	if err != nil {
		return nil, err
	}

	encrypt := true
	if pc.CollectionID != nil {
		col, err := s.q.GetCollectionForWrite(ctx, pgUUID(*pc.CollectionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCollectionNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("storage: get collection: %w", err)
		}
		if col.PublicArchival {
			encrypt = false
		}
	}

	var (
		stored  []byte
		wrapped []byte
		mkvID   uuid.UUID
	)
	if encrypt {
		pbk := make([]byte, envelope.KeySize)
		if _, err := rand.Read(pbk); err != nil {
			return nil, fmt.Errorf("storage: rand key: %w", err)
		}
		wrapped, mkvID, err = s.ks.Wrap(pbk)
		if err != nil {
			return nil, fmt.Errorf("storage: wrap: %w", err)
		}
		env, err := envelope.V1().Encrypt(buf, pbk)
		if err != nil {
			return nil, fmt.Errorf("storage: encrypt: %w", err)
		}
		stored = env
	} else {
		stored = buf
	}

	add, err := s.backend.AddDeterministic(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("storage: import: %w", err)
	}

	if err := s.commit(ctx, add, buf, mime, pc, encrypt, wrapped, mkvID); err != nil {
		if uerr := s.backend.Unpin(ctx, add.CID); uerr != nil {
			err = fmt.Errorf("%w (unpin also failed: %v)", err, uerr)
		}
		return nil, fmt.Errorf("storage: commit: %w", err)
	}

	return &PutResult{
		CID: add.CID.String(), ByteSize: int64(len(buf)),
		MIME: mime, Product: pc.Product, Encrypted: encrypt,
	}, nil
}

func (s *Service) commit(ctx context.Context, add ipfs.AddResult, buf []byte, mime string, pc PutContext, encrypt bool, wrapped []byte, mkvID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	cidStr := add.CID.String()

	var keyID pgtype.UUID // zero value ⟹ NULL (public_archival)
	if encrypt {
		id, err := qtx.InsertDEK(ctx, gen.InsertDEKParams{
			Algorithm: "XChaCha20-Poly1305", WrappedKey: wrapped, MasterKeyVersionID: pgUUID(mkvID),
		})
		if err != nil {
			return err
		}
		keyID = id
	}

	var owner pgtype.UUID
	if pc.OwnerID != nil {
		owner = pgUUID(*pc.OwnerID)
	}
	var srcIP *netip.Addr
	if pc.SourceIP.IsValid() {
		ip := pc.SourceIP
		srcIP = &ip
	}
	if err := qtx.InsertBlob(ctx, gen.InsertBlobParams{
		Cid: cidStr, EncryptionKeyID: keyID, OwnerID: owner,
		MimeType: mime, ByteSize: int64(len(buf)), SourceIp: srcIP,
		Product: gen.BlobProduct(pc.Product),
	}); err != nil {
		return err
	}

	var mr pgtype.Text
	if len(add.Blocks) > 1 {
		mr = pgtype.Text{String: add.MerkleRoot.String(), Valid: true}
	}
	if err := qtx.InsertManifest(ctx, gen.InsertManifestParams{
		Cid: cidStr, HashAlg: "sha2-256", Codec: add.Codec, Chunker: "size-262144",
		PlaintextSize: int64(len(buf)), EnvelopeSize: add.EnvelopeSize,
		BlockCount: int32(len(add.Blocks)), MerkleRoot: mr,
	}); err != nil {
		return err
	}
	for _, b := range add.Blocks {
		if err := qtx.InsertBlock(ctx, gen.InsertBlockParams{
			BlobCid: cidStr, BlockCid: b.CID.String(),
			BlockIndex: int32(b.Index), BlockSize: int32(b.Size),
		}); err != nil {
			return err
		}
	}
	if pc.CollectionID != nil {
		if err := qtx.InsertCollectionItem(ctx, gen.InsertCollectionItemParams{
			CollectionID: pgUUID(*pc.CollectionID), BlobCid: cidStr,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// pgUUID converts a google/uuid to pgtype.UUID (Valid).
func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
```

Add `"net/netip"` to `put.go` imports.

- [ ] **Step 6.4: Write the full fake backend + Put tests in `put_test.go`**

Complete the `fakeBackend` to satisfy `ipfs.Backend` (`Get` returns an `io.ReadCloser` over `store[cid]`; `Has`/`BlockstoreHas` return true if present; `Pin` no-op; `BlockGet` returns `store[cid]`; `Close` no-op). Then:

```go
func TestIntegrationPutEncryptedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	_, col := seedCollection(t, ctx, pool, false)

	body := []byte("hello nova upload")
	res, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col})
	require.NoError(t, err)
	require.True(t, res.Encrypted)
	require.Equal(t, int64(len(body)), res.ByteSize)

	// Stored bytes are an envelope (not the plaintext) because encryption ran.
	require.NotEqual(t, body, fb.store[res.CID])

	// Read back through Resolve/OpenBytes (the M3 read path).
	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	require.Equal(t, body, got)
}

func TestIntegrationPutPublicArchivalPlaintext(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, _ := envelope.NewKeystoreFromEnv(pool)
	_, _ = ks.Bootstrap(ctx)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	_, col := seedCollection(t, ctx, pool, true) // public_archival

	body := []byte("public data")
	res, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col})
	require.NoError(t, err)
	require.False(t, res.Encrypted)
	require.Equal(t, body, fb.store[res.CID]) // stored plaintext, no envelope
}

func TestIntegrationPutRollbackUnpinsOnCommitFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, _ := envelope.NewKeystoreFromEnv(pool)
	_, _ = ks.Bootstrap(ctx)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)

	// A non-existent collection makes commit's InsertCollectionItem fail the
	// FK; assert Unpin was called for the orphaned import.
	bogus := uuid.New()
	body := []byte("will roll back")
	_, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &bogus})
	require.ErrorIs(t, err, ErrCollectionNotFound) // resolved before import → no pin
	require.Empty(t, fb.unpinned)

	// Now force a commit-time failure: seed a collection, then drop the users
	// FK target by using an owner id that doesn't exist.
	_, col := seedCollection(t, ctx, pool, false)
	ghost := uuid.New()
	_, err = svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col, OwnerID: &ghost})
	require.Error(t, err)
	require.Len(t, fb.unpinned, 1) // orphan import unpinned
}

func TestIntegrationPutTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, _ := envelope.NewKeystoreFromEnv(pool)
	_, _ = ks.Bootstrap(ctx)
	svc := NewService(pool, newFakeBackend(), ks, WithWriteLimits(8, 4))
	_, err := svc.Put(ctx, bytes.NewReader([]byte("123456789")), 9, PutContext{MIME: "text/plain", Product: "raw"})
	require.ErrorIs(t, err, ErrUploadTooLarge)
}
```

- [ ] **Step 6.5: Run the storage tests**

Run: `go test ./pkg/coordinator/storage/ -run 'TestValidateMIME|TestIntegrationPut' -v`
Expected: PASS (testcontainer pull on first run).

- [ ] **Step 6.6: Update the `coordinator.New` call site if needed + build all**

Run: `go build ./...`
Expected: success (the M3 `NewService(pool, backend, ks)` call in `coordinator.go` still compiles — variadic).

- [ ] **Step 6.7: Commit**

```bash
git add pkg/coordinator/storage/
git commit -s -m "feat(storage): Service.Put write transaction (encrypt/public_archival, unpin-on-rollback, assembly semaphore)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 7: `internal/upload` session store

**Files:** Create `internal/upload/locks.go`, `internal/upload/store.go`, `internal/upload/store_test.go`.

- [ ] **Step 7.1: Implement the per-session lock map (`locks.go`)**

```go
package upload

import (
	"sync"

	"github.com/google/uuid"
)

// lockmap hands out one mutex per session id so concurrent PATCHes on the same
// session serialize (single-coordinator model, like tusd's MemoryLocker).
type lockmap struct {
	mu sync.Mutex
	m  map[uuid.UUID]*sync.Mutex
}

func newLockmap() *lockmap { return &lockmap{m: make(map[uuid.UUID]*sync.Mutex)} }

func (l *lockmap) get(id uuid.UUID) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m[id] == nil {
		l.m[id] = &sync.Mutex{}
	}
	return l.m[id]
}

func (l *lockmap) forget(id uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, id)
}
```

- [ ] **Step 7.2: Implement the store (`store.go`)**

```go
// Package upload owns the tus 1.0.0 (Creation + Core) resumable-upload session
// lifecycle: a Postgres upload_sessions row (offset/metadata, the source of
// truth) plus an on-disk chunk file under <dir>/<id>/data. Bytes accumulate on
// disk; finalize hands the assembled plaintext to a Committer (storage.Put).
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

var (
	ErrNotFound   = errors.New("upload: session not found")
	ErrConflict   = errors.New("upload: offset conflict")
	ErrIncomplete = errors.New("upload: offset != declared length")
	ErrTooLarge   = errors.New("upload: declared length exceeds max")
)

// Committer is the write surface finalize depends on (storage.Service).
type Committer interface {
	Put(ctx context.Context, r io.Reader, declaredSize int64, pc storage.PutContext) (*storage.PutResult, error)
}

// Store manages tus sessions.
type Store struct {
	q       *gen.Queries
	dir     string
	put     Committer
	ttl     time.Duration
	maxSize int64
	locks   *lockmap
}

// NewStore builds a session store. dir is the chunk root (created if absent).
func NewStore(pool *pgxpool.Pool, put Committer, dir string, ttl time.Duration, maxSize int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload: mkdir %s: %w", dir, err)
	}
	return &Store{q: gen.New(pool), dir: dir, put: put, ttl: ttl, maxSize: maxSize, locks: newLockmap()}, nil
}

// CreateParams carries validated tus-create metadata.
type CreateParams struct {
	DeclaredLength int64
	MIME           string
	Product        string
	CollectionID   *uuid.UUID
	OwnerID        *uuid.UUID
}

// Session is the offset/metadata view returned to handlers.
type Session struct {
	ID             uuid.UUID
	DeclaredLength int64
	OffsetBytes    int64
	MIME           string
	Product        string
	CollectionID   *uuid.UUID
	State          string
	BlobCID        string
}

func (s *Store) sessionDir(id uuid.UUID) string { return filepath.Join(s.dir, id.String()) }
func (s *Store) dataPath(id uuid.UUID) string   { return filepath.Join(s.sessionDir(id), "data") }

// Create inserts a session row and prepares its on-disk chunk file.
func (s *Store) Create(ctx context.Context, p CreateParams) (uuid.UUID, error) {
	if p.DeclaredLength > s.maxSize {
		return uuid.Nil, ErrTooLarge
	}
	var mime pgtype.Text
	if p.MIME != "" {
		mime = pgtype.Text{String: p.MIME, Valid: true}
	}
	product := p.Product
	if product == "" {
		product = "raw"
	}
	var owner, col pgtype.UUID
	if p.OwnerID != nil {
		owner = pgtype.UUID{Bytes: *p.OwnerID, Valid: true}
	}
	if p.CollectionID != nil {
		col = pgtype.UUID{Bytes: *p.CollectionID, Valid: true}
	}
	id, err := s.q.CreateUploadSession(ctx, gen.CreateUploadSessionParams{
		OwnerID: owner, DeclaredLength: p.DeclaredLength, MimeType: mime,
		Product: gen.BlobProduct(product), CollectionID: col,
		ExpiresAt: time.Now().Add(s.ttl),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload: create session: %w", err)
	}
	sid := uuid.UUID(id.Bytes)
	if err := os.MkdirAll(s.sessionDir(sid), 0o700); err != nil {
		return uuid.Nil, fmt.Errorf("upload: mkdir session: %w", err)
	}
	f, err := os.OpenFile(s.dataPath(sid), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload: create data file: %w", err)
	}
	_ = f.Close()
	return sid, nil
}

// Get loads a session view. Returns ErrNotFound when absent or aborted.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	row, err := s.q.GetUploadSession(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("upload: get session: %w", err)
	}
	if row.State == "aborted" {
		return nil, ErrNotFound
	}
	sess := &Session{
		ID: id, DeclaredLength: row.DeclaredLength, OffsetBytes: row.OffsetBytes,
		MIME: row.MimeType.String, Product: row.Product, State: row.State,
		BlobCID: row.BlobCid.String,
	}
	if row.CollectionID.Valid {
		c := uuid.UUID(row.CollectionID.Bytes)
		sess.CollectionID = &c
	}
	return sess, nil
}

// AppendChunk writes a chunk at clientOffset (idempotent seek-write) and
// advances the DB offset optimistically. Concurrent PATCHes on one session
// are serialized by an in-process lock; the loser gets ErrConflict.
func (s *Store) AppendChunk(ctx context.Context, id uuid.UUID, clientOffset int64, r io.Reader) (int64, error) {
	mu := s.locks.get(id)
	if !mu.TryLock() {
		return 0, ErrConflict // a concurrent PATCH holds the session
	}
	defer mu.Unlock()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if sess.State != "in_progress" || sess.OffsetBytes != clientOffset {
		return 0, ErrConflict
	}

	f, err := os.OpenFile(s.dataPath(id), os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("upload: open data: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(clientOffset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("upload: seek: %w", err)
	}
	remaining := sess.DeclaredLength - clientOffset
	n, err := io.Copy(f, io.LimitReader(r, remaining))
	if err != nil {
		return 0, fmt.Errorf("upload: write chunk: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("upload: fsync: %w", err)
	}

	newOffset := clientOffset + n
	rows, err := s.q.AdvanceUploadOffset(ctx, gen.AdvanceUploadOffsetParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, OffsetBytes: newOffset, OffsetBytes_2: clientOffset,
	})
	if err != nil {
		return 0, fmt.Errorf("upload: advance offset: %w", err)
	}
	if rows == 0 {
		return 0, ErrConflict // someone advanced it concurrently / state changed
	}
	return newOffset, nil
}

// Finalize commits the assembled bytes via the Committer. Idempotent: a second
// finalize of a finalized session returns its existing result fields.
func (s *Store) Finalize(ctx context.Context, id uuid.UUID) (*storage.PutResult, error) {
	mu := s.locks.get(id)
	mu.Lock()
	defer mu.Unlock()

	sess, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.State == "finalized" {
		return &storage.PutResult{
			CID: sess.BlobCID, ByteSize: sess.DeclaredLength,
			MIME: sess.MIME, Product: sess.Product, Encrypted: sess.CollectionID == nil || true,
		}, nil
	}
	if sess.OffsetBytes != sess.DeclaredLength {
		return nil, ErrIncomplete
	}

	f, err := os.Open(s.dataPath(id))
	if err != nil {
		return nil, fmt.Errorf("upload: open assembled: %w", err)
	}
	defer f.Close()

	res, err := s.put.Put(ctx, f, sess.DeclaredLength, storage.PutContext{
		MIME: sess.MIME, Product: sess.Product, CollectionID: sess.CollectionID, OwnerID: nil,
	})
	if err != nil {
		return nil, err
	}
	if err := s.q.FinalizeUploadSession(ctx, gen.FinalizeUploadSessionParams{
		ID: pgtype.UUID{Bytes: id, Valid: true}, BlobCid: pgtype.Text{String: res.CID, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("upload: mark finalized: %w", err)
	}
	_ = os.RemoveAll(s.sessionDir(id))
	s.locks.forget(id)
	return res, nil
}

// Abort marks the session aborted and removes its chunk dir.
func (s *Store) Abort(ctx context.Context, id uuid.UUID) error {
	mu := s.locks.get(id)
	mu.Lock()
	defer mu.Unlock()
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.q.AbortUploadSession(ctx, pgtype.UUID{Bytes: id, Valid: true}); err != nil {
		return fmt.Errorf("upload: abort: %w", err)
	}
	_ = os.RemoveAll(s.sessionDir(id))
	s.locks.forget(id)
	return nil
}

// GC removes abandoned in_progress sessions past their TTL. Filesystem
// cleanup precedes the row delete so a crash mid-sweep leaves the row (retried
// next tick), never an orphaned directory.
func (s *Store) GC(ctx context.Context) (int, error) {
	ids, err := s.q.ListExpiredUploadSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("upload: list expired: %w", err)
	}
	n := 0
	for _, pgid := range ids {
		id := uuid.UUID(pgid.Bytes)
		_ = os.RemoveAll(s.sessionDir(id))
		if err := s.q.DeleteUploadSession(ctx, pgid); err != nil {
			return n, fmt.Errorf("upload: delete session: %w", err)
		}
		s.locks.forget(id)
		n++
	}
	return n, nil
}
```

> Note on sqlc param names: a query with two `$N` bindings to the same column (`offset_bytes = $2 ... AND offset_bytes = $3`) generates fields `OffsetBytes` and `OffsetBytes_2`. If sqlc names them differently after generation, adjust the struct literal in `AppendChunk` accordingly (check `internal/db/gen/uploads.sql.go`).

- [ ] **Step 7.3: Write store tests (`store_test.go`)**

Cover, against `dbtest.New` + a temp dir + a fake `Committer` that returns a fixed `PutResult`:
- create → Get offset 0 → AppendChunk(0, "abc") → offset 3 → AppendChunk(3, "de") → offset 5 → Finalize → result CID; data file content == "abcde" was passed to the committer.
- AppendChunk with wrong clientOffset → `ErrConflict`.
- Finalize before complete → `ErrIncomplete`.
- concurrent AppendChunk (two goroutines at offset 0) → exactly one success, one `ErrConflict`; file not corrupted.
- re-Finalize a finalized session → same CID, no second `Put` call (fake counts calls).
- Abort → subsequent Get → `ErrNotFound`; dir gone.
- GC: insert an expired in_progress row (via direct SQL with `expires_at = now() - interval '1h'`) → GC returns 1, row gone, dir gone; a finalized session is untouched.

```go
package upload_test

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeCommitter struct {
	calls   atomic.Int32
	lastLen int64
	lastBuf []byte
}

func (f *fakeCommitter) Put(ctx context.Context, r io.Reader, n int64, pc storage.PutContext) (*storage.PutResult, error) {
	f.calls.Add(1)
	b, _ := io.ReadAll(r)
	f.lastBuf = b
	f.lastLen = n
	return &storage.PutResult{CID: "bafytestcid", ByteSize: n, MIME: pc.MIME, Product: pc.Product, Encrypted: true}, nil
}

func newStore(t *testing.T, ctx context.Context, fc *fakeCommitter) *upload.Store {
	t.Helper()
	pool := dbtest.New(t, ctx)
	st, err := upload.NewStore(pool, fc, t.TempDir(), time.Hour, 1024)
	require.NoError(t, err)
	return st
}

func TestIntegrationUploadHappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	fc := &fakeCommitter{}
	st := newStore(t, ctx, fc)
	id, err := st.Create(ctx, upload.CreateParams{DeclaredLength: 5, MIME: "text/plain", Product: "raw"})
	require.NoError(t, err)

	off, err := st.AppendChunk(ctx, id, 0, mkReader("abc"))
	require.NoError(t, err)
	require.Equal(t, int64(3), off)
	off, err = st.AppendChunk(ctx, id, 3, mkReader("de"))
	require.NoError(t, err)
	require.Equal(t, int64(5), off)

	res, err := st.Finalize(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "bafytestcid", res.CID)
	require.Equal(t, []byte("abcde"), fc.lastBuf)

	// idempotent re-finalize, no second Put
	_, err = st.Finalize(ctx, id)
	require.NoError(t, err)
	require.Equal(t, int32(1), fc.calls.Load())
}
```

(Add `mkReader` helper returning a `*strings.Reader`, plus the offset-conflict, incomplete, concurrent, abort, and GC tests described above. The concurrent test launches two goroutines calling `AppendChunk(ctx, id, 0, ...)` and asserts exactly one returns nil.)

- [ ] **Step 7.4: Run upload tests**

Run: `go test ./internal/upload/... -v`
Expected: PASS.

- [ ] **Step 7.5: Commit**

```bash
git add internal/upload/
git commit -s -m "feat(upload): tus session store (DB offset + disk chunks, in-proc lock, optimistic advance, GC)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 8: `derivative_prewarm` stub

**Files:** Create `internal/jobs/kinds/derivative_prewarm.go`, `internal/jobs/kinds/derivative_prewarm_test.go`.

- [ ] **Step 8.1: Implement the stub**

```go
// Package kinds holds the job-kind constants and handlers registered with the
// worker pool. derivative_prewarm is a no-op in Phase 1 M4; nova-image wires
// the real body and the enqueue site in M5 (OnCommitted).
package kinds

import "context"

// KindDerivativePrewarm pre-warms common image-derivative presets after a
// parent upload commits. M4 ships the kind so M5 only fills the body.
const KindDerivativePrewarm = "derivative_prewarm"

// DerivativePrewarmStub is the M4 no-op handler.
func DerivativePrewarmStub(ctx context.Context, payload []byte) error { return nil }
```

- [ ] **Step 8.2: Test**

```go
package kinds

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerivativePrewarmStubIsNoop(t *testing.T) {
	require.NoError(t, DerivativePrewarmStub(context.Background(), []byte(`{"cid":"bafy"}`)))
	require.Equal(t, "derivative_prewarm", KindDerivativePrewarm)
}
```

- [ ] **Step 8.3: Run + commit**

Run: `go test ./internal/jobs/kinds/...`
Expected: PASS.

```bash
git add internal/jobs/kinds/
git commit -s -m "feat(jobs): derivative_prewarm kind constant + no-op stub (M5 fills body)" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 9: tus + multipart HTTP handlers

**Files:** Create `internal/api/handlers/upload.go`, `internal/api/handlers/upload_test.go`.

- [ ] **Step 9.1: Implement the handler (`upload.go`)**

```go
package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/upload"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

const tusVersion = "1.0.0"

// SessionStore is the tus surface the handler depends on.
type SessionStore interface {
	Create(ctx context.Context, p upload.CreateParams) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (*upload.Session, error)
	AppendChunk(ctx context.Context, id uuid.UUID, clientOffset int64, r io.Reader) (int64, error)
	Finalize(ctx context.Context, id uuid.UUID) (*storage.PutResult, error)
	Abort(ctx context.Context, id uuid.UUID) error
}

// UploadHandler serves tus (/api/v1/uploads*) and multipart (/api/v1/blobs).
type UploadHandler struct {
	store         SessionStore
	put           upload.Committer
	maxUploadSize int64
	recordIP      bool
}

func NewUploadHandler(store SessionStore, put upload.Committer, maxUploadSize int64, recordIP bool) *UploadHandler {
	return &UploadHandler{store: store, put: put, maxUploadSize: maxUploadSize, recordIP: recordIP}
}

func (h *UploadHandler) CreateTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	if r.Header.Get("Tus-Resumable") != tusVersion {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "Tus-Resumable: 1.0.0 required", rid)
		return
	}
	length, err := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "valid Upload-Length required", rid)
		return
	}
	if length > h.maxUploadSize {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
		return
	}
	meta := parseUploadMetadata(r.Header.Get("Upload-Metadata"))
	p := upload.CreateParams{DeclaredLength: length, MIME: meta["mime_type"], Product: meta["product"]}
	if cidStr := meta["collection_id"]; cidStr != "" {
		cid, err := uuid.Parse(cidStr)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "bad_request", "collection_id must be a uuid", rid)
			return
		}
		p.CollectionID = &cid
	}
	id, err := h.store.Create(r.Context(), p)
	if err != nil {
		if errors.Is(err, upload.ErrTooLarge) {
			httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Location", "/api/v1/uploads/"+id.String())
	w.Header().Set("Upload-Offset", "0")
	w.WriteHeader(http.StatusCreated)
}

func (h *UploadHandler) HeadTus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	sess, err := h.store.Get(r.Context(), id)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(sess.OffsetBytes, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(sess.DeclaredLength, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (h *UploadHandler) PatchTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "Content-Type must be application/offset+octet-stream", rid)
		return
	}
	clientOffset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || clientOffset < 0 {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "valid Upload-Offset required", rid)
		return
	}
	newOffset, err := h.store.AppendChunk(r.Context(), id, clientOffset, r.Body)
	switch {
	case errors.Is(err, upload.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return
	case errors.Is(err, upload.ErrConflict):
		httputil.WriteError(w, http.StatusConflict, "offset_conflict", "upload offset conflict", rid)
		return
	case err != nil:
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (h *UploadHandler) DeleteTus(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if err := h.store.Abort(r.Context(), id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *UploadHandler) FinalizeTus(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	res, err := h.store.Finalize(r.Context(), id)
	switch {
	case errors.Is(err, upload.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
		return
	case errors.Is(err, upload.ErrIncomplete):
		httputil.WriteError(w, http.StatusConflict, "upload_incomplete", "upload not yet complete", rid)
		return
	case err != nil:
		h.writePutError(w, err, rid)
		return
	}
	writeUploadResult(w, res)
}

func (h *UploadHandler) Multipart(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	if err := r.ParseMultipartForm(8 << 20); err != nil { // 8 MiB in-memory; rest spills to disk
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "invalid multipart form", rid)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "bad_request", "file field required", rid)
		return
	}
	defer file.Close()
	if header.Size > h.maxUploadSize {
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
		return
	}
	product := r.FormValue("product")
	if product == "" {
		product = "raw"
	}
	pc := storage.PutContext{MIME: header.Header.Get("Content-Type"), Product: product}
	if h.recordIP {
		pc.SourceIP = clientIP(r)
	}
	if cidStr := r.FormValue("collection_id"); cidStr != "" {
		cid, err := uuid.Parse(cidStr)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "bad_request", "collection_id must be a uuid", rid)
			return
		}
		pc.CollectionID = &cid
	}
	res, err := h.put.Put(r.Context(), file, header.Size, pc)
	if err != nil {
		h.writePutError(w, err, rid)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeUploadResultBody(w, res)
}

func (h *UploadHandler) writePutError(w http.ResponseWriter, err error, rid string) {
	switch {
	case errors.Is(err, storage.ErrUploadTooLarge):
		httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds max size", rid)
	case errors.Is(err, storage.ErrMimeRejected):
		httputil.WriteError(w, http.StatusBadRequest, "mime_rejected", "declared content-type contradicts content", rid)
	case errors.Is(err, storage.ErrCollectionNotFound):
		httputil.WriteError(w, http.StatusNotFound, "not_found", "collection not found", rid)
	case errors.Is(err, storage.ErrServerBusy):
		w.Header().Set("Retry-After", "2")
		httputil.WriteError(w, http.StatusServiceUnavailable, "server_busy", "server at capacity, retry", rid)
	default:
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
	}
}

// parseUploadMetadata decodes the tus Upload-Metadata header
// ("key b64val,key2 b64val2"). Unknown/blank keys (e.g. filename) are ignored.
func parseUploadMetadata(h string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(h, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, " ", 2)
		key := kv[0]
		if len(kv) == 2 {
			if dec, err := base64.StdEncoding.DecodeString(kv[1]); err == nil {
				out[key] = string(dec)
			}
		} else {
			out[key] = ""
		}
	}
	return out
}

func parseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func clientIP(r *http.Request) (a netipAddr) {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if ip, err := parseAddr(first); err == nil {
			return ip
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	ip, _ := parseAddr(host)
	return ip
}
```

Use `net/netip` directly (the `netipAddr`/`parseAddr` aliases above are placeholders to keep this block readable): import `"net/netip"`, declare `clientIP` to return `netip.Addr`, and call `netip.ParseAddr`. Add the result writers in the same file:

```go
func writeUploadResult(w http.ResponseWriter, res *storage.PutResult) {
	w.WriteHeader(http.StatusOK)
	writeUploadResultBody(w, res)
}

func writeUploadResultBody(w http.ResponseWriter, res *storage.PutResult) {
	w.Header().Set("X-Nova-Cid", res.CID)
	w.Header().Set("X-Nova-Envelope-Version", "1")
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"cid": res.CID, "byte_size": res.ByteSize, "mime_type": res.MIME, "product": res.Product,
		"urls": map[string]string{"original": "/blob/" + res.CID, "json": "/blob/" + res.CID + ".json"},
	}
	_ = jsonEncode(w, body)
}
```

> Implementation note: replace the `netipAddr`/`parseAddr`/`jsonEncode` readability placeholders with the real stdlib calls (`netip.Addr`, `netip.ParseAddr`, `json.NewEncoder(w).Encode`) when writing the file; set `Content-Type`/headers **before** `WriteHeader` (fix the ordering in `writeUploadResultBody` so headers precede the status write — see Step 9.2 test which asserts the JSON body + 201).

- [ ] **Step 9.2: Write handler tests (`upload_test.go`)**

Using `httptest` + a fake `SessionStore` and a fake `upload.Committer`, assert:
- `CreateTus` without `Tus-Resumable` → 400; with valid headers → 201 + `Location` + `Upload-Offset: 0`; `Upload-Length` over max → 413.
- `parseUploadMetadata("filename ZmlsZQ==,mime_type aW1hZ2UvanBlZw==")` → `{filename:"file", mime_type:"image/jpeg"}` (table test).
- `PatchTus` wrong content-type → 400; store `ErrConflict` → 409; success → 204 + `Upload-Offset`.
- `FinalizeTus` `ErrIncomplete` → 409; success → 200 + JSON body with `cid`/`urls.original`.
- `Multipart` happy path → 201 + JSON; `ErrMimeRejected` from committer → 400; `ErrServerBusy` → 503.

- [ ] **Step 9.3: Run handler tests**

Run: `go test ./internal/api/handlers/... -v`
Expected: PASS.

- [ ] **Step 9.4: Commit**

```bash
git add internal/api/handlers/upload.go internal/api/handlers/upload_test.go
git commit -s -m "feat(api): tus + multipart upload handlers" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 10: Mount routes + wire coordinator + GC ticker + cmd env

**Files:** Modify `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`.

- [ ] **Step 10.1: Mount upload routes in `server.go`**

Add `Upload *handlers.UploadHandler` to `ServerConfig`, and in `NewServer` after the blob routes:

```go
	if cfg.Upload != nil {
		r.Route("/api/v1/uploads", func(r chi.Router) {
			r.Post("/", cfg.Upload.CreateTus)
			r.Route("/{id}", func(r chi.Router) {
				r.Head("/", cfg.Upload.HeadTus)
				r.Patch("/", cfg.Upload.PatchTus)
				r.Delete("/", cfg.Upload.DeleteTus)
				r.Post("/finalize", cfg.Upload.FinalizeTus)
			})
		})
		r.Post("/api/v1/blobs", cfg.Upload.Multipart)
	}
```

- [ ] **Step 10.2: Wire the store + handler + GC ticker in `coordinator.go`**

Extend `Config`:

```go
	// Upload knobs (M4). When TmpDir is set and deps are present, the write
	// path (tus + multipart) is mounted and a session-GC ticker runs.
	MaxUploadSizeBytes    int64
	MaxConcurrentAssembly int
	SessionTTL            time.Duration
	UploadTmpDir          string
	UploadGCInterval      time.Duration
	RecordSourceIP        bool
```

Extend `Coordinator` with `uploadStore *upload.Store` and `gcInterval time.Duration`. In `New`, when `pool/backend/ks` present **and** `cfg.UploadTmpDir != ""`:

```go
		svc := storage.NewService(pool, backend, ks,
			storage.WithWriteLimits(cfg.MaxUploadSizeBytes, cfg.MaxConcurrentAssembly))
		sc.Blob = handlers.NewBlobHandler(svc)

		store, err := upload.NewStore(pool, svc, cfg.UploadTmpDir, cfg.SessionTTL, sizeOrDefault(cfg.MaxUploadSizeBytes))
		if err != nil {
			return nil, err
		}
		uploadStore = store
		sc.Upload = handlers.NewUploadHandler(store, svc, sizeOrDefault(cfg.MaxUploadSizeBytes), cfg.RecordSourceIP)
```

(Keep the existing read-only branch — `NewService(pool, backend, ks)` + blob handler — for when `UploadTmpDir` is empty, so M3 lifecycle tests are unaffected. Store `uploadStore`/`gcInterval` on the returned `Coordinator`. Add a `sizeOrDefault` helper returning `104857600` when `<= 0`.)

In `Run`, after `c.addr.Store(...)`, start the GC ticker:

```go
	if c.uploadStore != nil {
		interval := c.gcInterval
		if interval <= 0 {
			interval = time.Hour
		}
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_, _ = c.uploadStore.GC(ctx)
				}
			}
		}()
	}
```

Add imports: `"github.com/nova-archive/nova/internal/upload"`, `"time"` (already present).

- [ ] **Step 10.3: Wire env knobs in `cmd/coordinator/main.go`**

After the existing env parsing, add (with `internal/config` for default constants):

```go
	tmpDir := os.Getenv("NOVA_UPLOAD_TMP_DIR")
	if tmpDir == "" {
		tmpDir = filepath.Join(os.TempDir(), "nova-uploads")
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("create upload tmp dir: %w", err)
	}
	maxUpload := envInt64("NOVA_MAX_UPLOAD_SIZE_BYTES", config.DefaultMaxUploadSizeBytes)
	maxAssembly := envInt("NOVA_MAX_CONCURRENT_ASSEMBLY", config.DefaultMaxConcurrentAssembly)
	sessionTTL := time.Duration(envInt("NOVA_UPLOAD_SESSION_TTL_SECONDS", config.DefaultUploadSessionTTLSecs)) * time.Second
	recordIP := os.Getenv("NOVA_PARANOID") != "true"
```

Pass into `coordinator.Config`:

```go
		MaxUploadSizeBytes:    maxUpload,
		MaxConcurrentAssembly: maxAssembly,
		SessionTTL:            sessionTTL,
		UploadTmpDir:          tmpDir,
		UploadGCInterval:      time.Hour,
		RecordSourceIP:        recordIP,
```

Add small `envInt`/`envInt64` helpers (parse with fallback) and imports `"path/filepath"`, `"strconv"`, `"github.com/nova-archive/nova/internal/config"`.

- [ ] **Step 10.4: Build everything**

Run: `go build ./...`
Expected: success.

- [ ] **Step 10.5: Run the coordinator + api + storage tests**

Run: `go test ./pkg/coordinator/... ./internal/api/... -run '.*' -short`
Expected: PASS (short-mode skips the heavy integration tests; lifecycle/handler unit tests run).

- [ ] **Step 10.6: Commit**

```bash
git add internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go
git commit -s -m "feat(coordinator): mount tus + multipart routes, wire upload store + GC ticker, env knobs" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 11: dev nginx pass-through + integration test

**Files:** Modify `docker/nginx/nova.dev.conf`; Create `internal/integration/m4_upload_test.go`.

- [ ] **Step 11.1: Add upload locations to `docker/nginx/nova.dev.conf`**

Add inside the server block (alongside the existing `/blob/` location), disabling request buffering so tus PATCH streams:

```nginx
  location /api/v1/uploads { proxy_pass http://coordinator:9000; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; proxy_set_header X-Request-ID $request_id; }
  location = /api/v1/blobs { proxy_pass http://coordinator:9000; proxy_request_buffering off; proxy_set_header X-Forwarded-For $remote_addr; proxy_set_header X-Request-ID $request_id; }
```

(Match the existing upstream name/style already in the file.)

- [ ] **Step 11.2: Write the integration test (`m4_upload_test.go`)**

Mirror `m3_read_api_test.go`'s harness (`dbtest.New`, keystore, embedded backend offline, `coordinator.New` with upload config + a `t.TempDir()` `UploadTmpDir`, `startNginxM4` whose conf also proxies `/api/v1/`). The test:

```go
package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
)

func TestIntegrationM4UploadThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M4 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = backend.Close(c)
	})

	const coordPort = "19004"
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr:            "0.0.0.0:" + coordPort,
		Version:               "m4-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    1 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	base := startNginxUploads(t, ctx, coordPort) // extends startNginx with /api/v1/

	// Seed a public collection so uploaded blobs are anonymously readable.
	col := seedPublicCollection(t, ctx, pool)

	fixtures := map[string][]byte{
		"image/jpeg": {0xff, 0xd8, 0xff, 0xe0, 'J', 'F', 'I', 'F', 1, 2, 3, 4},
		"image/png":  {0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 9, 8, 7},
		"image/webp": append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("WEBPVP8 hello")...),
	}

	for mime, body := range fixtures {
		// --- tus path (two chunks for jpeg to exercise resumption) ---
		cid := tusUpload(t, base, mime, col, body, mime == "image/jpeg")
		assertFetch(t, base, cid, body, mime)

		// --- multipart path ---
		cid2 := multipartUpload(t, base, mime, col, body)
		assertFetch(t, base, cid2, body, mime)
	}

	// Negative: oversize create → 413.
	requireTusCreateStatus(t, base, mime("text/plain"), col, 2<<20, 413)

	// Negative: bad MIME (declare image/jpeg, send a script) → 400 at finalize.
	requireBadMimeFinalize(t, base, col)
}
```

Implement the helpers in the same file: `seedPublicCollection` (insert user + public collection, return id); `tusUpload` (POST create with `Upload-Metadata` = base64 of `mime_type` + `collection_id`, PATCH the body in one or two chunks, POST finalize, parse `UploadResult.cid`); `multipartUpload` (build a `multipart.Writer` with `file`, set the part's `Content-Type` to `mime`, add `collection_id` field, POST to `/api/v1/blobs`, parse cid); `assertFetch` (GET `/blob/{cid}` → 200, byte-equal, `Content-Type`); `startNginxUploads` (same as M3's `startNginx` but the conf adds `location /api/v1/ { proxy_pass ...; proxy_request_buffering off; }`); `requireTusCreateStatus` and `requireBadMimeFinalize`. Reuse `atoiPort` from the M3 file (same package).

> tus metadata encoding: `Upload-Metadata: mime_type <b64(mime)>,collection_id <b64(col)>` using `base64.StdEncoding`. PATCH headers: `Tus-Resumable: 1.0.0`, `Upload-Offset: <n>`, `Content-Type: application/offset+octet-stream`.

- [ ] **Step 11.3: Run the M4 integration test**

Run: `go test ./internal/integration/ -run TestIntegrationM4Upload -v`
Expected: PASS (tus + multipart for all three formats round-trip; negatives return 413/400).

- [ ] **Step 11.4: Run the full suite (short) + lint**

Run: `go build ./... && go test ./... -short && golangci-lint run`
Expected: build clean; short tests PASS; lint clean.

- [ ] **Step 11.5: Commit**

```bash
git add docker/nginx/nova.dev.conf internal/integration/m4_upload_test.go
git commit -s -m "test(m4): nginx-fronted tus + multipart round-trip (jpeg/png/webp); dev nginx passes upload routes" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 12: Doc reconciliations + mark M4 in progress

**Files:** Modify `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`, `docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md`.

- [ ] **Step 12.1: Reconcile the Phase-1 design M4 line**

In `…single-node-mvp-design.md` under "**M4 — Upload pipeline (~week 4)**", change the bullet `nova-image AnalyzeUpload (width/height/PDQ); no format conversion yet.` to:

```
- Product-agnostic write path; the AnalyzeUpload seam is a no-op in M4.
  nova-image AnalyzeUpload (width/height/PDQ) moves to M5 (see
  docs/superpowers/specs/2026-05-29-phase1-m4-upload-pipeline-design.md
  § "Source of truth and required doc reconciliations").
```

- [ ] **Step 12.2: Update the master plan milestone table + M4 summary**

In `…single-node-mvp.md`: set the M4 row Status to **in progress** and Plan to a link to `2026-05-29-phase1-m4-upload-pipeline.md`. In the M4 deliverables summary, change `multipart fallback (/api/v1/blobs, /api/v1/images)` to `multipart fallback (/api/v1/blobs)` and append: `(/api/v1/images moves to M5 with the nova-image product.)`

- [ ] **Step 12.3: Commit**

```bash
git add docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md docs/superpowers/plans/2026-05-25-phase1-single-node-mvp.md
git commit -s -m "docs(m4): mark M4 in progress; reconcile nova-image AnalyzeUpload + /api/v1/images to M5" \
  -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Final verification

- [ ] `go build ./...` — clean.
- [ ] `make codegen-check` — no sqlc drift.
- [ ] `go test ./...` — all pass (integration tests included; ~several minutes for testcontainers).
- [ ] `golangci-lint run` — clean.
- [ ] Manual exit check (matches the spec exit criteria): the M4 integration test uploads JPEG/PNG/WebP via both tus and multipart, each fetchable byte-equal through nginx, each with `blob_manifests` + `blob_blocks` rows.

---

## Self-review notes (against the spec)

- **Spec coverage:** §1 scope (product-agnostic, tus+multipart, no nova-image) → Tasks 6/9/12. §3 `upload_sessions` → Task 1. §4 write pipeline (encrypt/public_archival/unpin/crash-gap/dedup/semaphore) → Task 6. §5 tus surface + concurrency + multipart + size → Tasks 7/9. §6 HTTP contract + auth posture → Task 9 (+ `nova_dev` floor unchanged from M3). §7 config → Task 2. §8 derivative_prewarm stub → Task 8. §"Stale-session GC" → Tasks 7/10. §"MIME validation" → Task 5. §Testing → Tasks 6/7/9/11. §reconciliations → Task 12.
- **`source_ip`:** recorded in the multipart path via `clientIP`; tus path leaves `OwnerID`/`SourceIP` nil in M4 (Finalize passes `OwnerID: nil`; source IP for tus is captured at create-time only if needed — deferred, the row carries no source_ip in M4 tus path, acceptable since both are anonymous/NULL until M6). Paranoid (`NOVA_PARANOID=true`) sets `RecordSourceIP=false`.
- **`netip`/`json` placeholders in Task 9** are flagged inline to be replaced with real stdlib calls; ensure header writes precede `WriteHeader`.
- **sqlc duplicate-param name** (`OffsetBytes_2`) flagged in Task 7 — verify against generated code.
