# Phase 1 M3 — Storage Core API (Read Path) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up a coordinator that serves the anonymous read path — `GET /health`, `GET/HEAD /blob/{cid}`, `GET /blob/{cid}.json` — returning decrypted bytes through an nginx proxy, composing the M2 envelope/IPFS/keystore subsystems behind an HTTP-naïve storage service under a stable `pkg/coordinator` lifecycle.

**Architecture:** `cmd/coordinator` performs manual dependency injection (open pool, build + validate embedded Kubo, bootstrap keystore) and constructs `pkg/coordinator`, which owns an `http.Server`. `internal/api` chi handlers call `pkg/coordinator/storage.Service`, which resolves a blob row (sqlc/Postgres), fetches its envelope (Kubo), unwraps the per-blob key (keystore), and decrypts (envelope v1). The storage service returns typed domain errors; handlers map them to per-endpoint HTTP statuses.

**Tech Stack:** Go 1.25, `github.com/go-chi/chi/v5`, sqlc (`pgx/v5` codegen, committed `gen/`), `github.com/jackc/pgx/v5`, `github.com/google/uuid`, `github.com/ipfs/go-cid`, testcontainers-go (Postgres + nginx). Builds on M2 `internal/envelope`, `internal/ipfs`, `internal/db`.

**Author:** Bug Plowman (operator), Claude (implementation partner).

**Status:** in progress — executing on branch `m3-storage-read-api`.

**Spec:** `docs/superpowers/specs/phase1/2026-05-28-phase1-m3-storage-read-api-design.md` (authoritative design).

---

## Conventions for this plan

- **Commits:** every commit uses `git commit -s` (sign-off) and includes the trailer `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` (repo convention; see `git log`). Commit subjects below omit the trailer for brevity — always add it.
- **Module path:** `github.com/nova-archive/nova`.
- **Run tests:** `go test ./<pkg>/... -count=1`. Integration tests are gated by `testing.Short()`; run them with `go test ./internal/integration/... -run Integration -count=1` (no `-short`).
- **Branch:** all work lands on `m3-storage-read-api`; the spec + openapi reconciliations are already committed on `main` and inherited here.

---

## Preconditions from M2 (verify before starting)

These symbols must exist as described (they do as of the M2 tag). The plan's code calls them directly:

- `envelope.Decode(b []byte) (byte, envelope.Codec, error)`, `envelope.Codec.Decrypt(env, key []byte) ([]byte, error)`, `envelope.V1()`, `envelope.KeySize`.
- `envelope.NewKeystoreFromEnv(pool) (*Keystore, error)`, `(*Keystore).Bootstrap(ctx) (uuid.UUID, error)`, `(*Keystore).Wrap(pbk) ([]byte, uuid.UUID, error)`, `(*Keystore).Unwrap(wrapped []byte, versionID uuid.UUID) ([]byte, error)`.
- `ipfs.Backend` interface with `Get(ctx, cid.Cid) (io.ReadCloser, error)` and `AddDeterministic(ctx, []byte) (ipfs.AddResult, error)`; `ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{RepoPath, Mode, SwarmKeyPath, Online}) (*EmbeddedBackend, error)`; `ipfs.ModePrivate`; `ipfs.WriteFileForTest(path, data)`.
- `db.Open(ctx, dsn) (*pgxpool.Pool, error)`.
- `dbtest.New(t, ctx) *pgxpool.Pool` (testcontainer Postgres + migrations).
- Migrations 0001–0004 applied, including `blobs.envelope_version smallint NOT NULL DEFAULT 1`.

---

## File structure

### Created

| Path | Responsibility |
|---|---|
| `internal/db/sqlc.yaml` | sqlc config (pgx/v5, goose-aware schema, timestamptz override) |
| `internal/db/queries/blobs.sql` | `GetBlobCore`, `GetDEKByBlob`, `GetManifestSize` |
| `internal/db/queries/collections.sql` | `ResolveBlobVisibility` |
| `internal/db/gen/*.go` | committed sqlc output (`db.go`, `models.go`, `*.sql.go`, `querier.go`) |
| `pkg/coordinator/storage/errors.go` | domain sentinel errors |
| `pkg/coordinator/storage/types.go` | `Visibility`, `BlobView`, `resolveVisibility` |
| `pkg/coordinator/storage/blob.go` | `Service`, `NewService`, `Resolve`, `OpenBytes` |
| `pkg/coordinator/storage/*_test.go` | storage unit/integration tests |
| `internal/blobfixture/blobfixture.go` | test helper: seed a blob end-to-end (encrypt→import→rows) |
| `internal/api/errors.go` | `Error` JSON model + `WriteError` |
| `internal/api/middleware/requestid.go` | `X-Request-ID` middleware + context accessor |
| `internal/api/middleware/recover.go` | panic → 500 (no stack leak) |
| `internal/api/middleware/ratelimit.go` | per-IP token-bucket middleware |
| `internal/ratelimit/bucket.go` | token bucket + keyed limiter |
| `internal/api/handlers/health.go` | `GET /health` |
| `internal/api/handlers/blob.go` | `GET/HEAD /blob/{cid}`, `GET /blob/{cid}.json` |
| `internal/api/server.go` | chi router assembly + namespace reservation |
| `internal/api/*_test.go`, `handlers/*_test.go`, `middleware/*_test.go` | HTTP-layer tests |
| `internal/auth/anonymous_prod.go` | `//go:build !nova_dev` refuse-to-start floor |
| `internal/auth/anonymous_dev.go` | `//go:build nova_dev` bypass |
| `internal/auth/anonymous_test.go` | floor tests |
| `pkg/coordinator/coordinator.go` | `Config`, `New`, `Run`, `Shutdown` |
| `pkg/coordinator/coordinator_test.go` | lifecycle test |
| `cmd/coordinator/main.go` | wiring + startup validation + SIGTERM |
| `docker/nginx/nova.dev.conf` | minimal dev proxy |
| `internal/integration/m3_read_api_test.go` | nginx-fronted E2E read test |

### Modified

| Path | Why |
|---|---|
| `go.mod` / `go.sum` | add `github.com/go-chi/chi/v5`; sqlc anchor |
| `tools.go` | anchor pinned sqlc generator |
| `Makefile` | `sqlc-generate`, `codegen-check`, `build-coordinator`, `run-coordinator` |
| `.github/workflows/ci.yml` | add `codegen-check` gate |
| `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md` | mark M3 status; link this plan |

---

## Architectural notes for the implementer

- **`storage.Service` is HTTP-naïve.** It returns the sentinels in `errors.go`; only `internal/api` knows status codes. The 401-vs-404 difference for private blobs is applied **in the handler**, per endpoint (bytes→401, `.json`→404), from the *same* `ErrBlobAuthRequired`.
- **Queries cast enums and UUIDs to `text`** (`state::text`, `master_key_version_id::text`) so generated types are plain `string`/`[]byte`/`bool`, sidestepping pgx enum-OID codec issues. `uuid.Parse` converts where a real UUID is needed (keystore `Unwrap`). `timestamptz` is overridden to `time.Time`.
- **Read uses small composable queries**, not one big LEFT JOIN: `GetBlobCore` (with `encryption_key_id IS NOT NULL AS encrypted`), then `GetDEKByBlob` only when encrypted, `GetManifestSize`, `ResolveBlobVisibility`. Acceptable round-trips for M3 (nginx caches reads later).
- **`pkg/coordinator.New` takes injected deps** (`*pgxpool.Pool`, `ipfs.Backend`, `*envelope.Keystore`, `Config`). `RegisterProduct` is deferred to M5; the router is structured so product sub-routers mount later, and product/storage namespaces are reserved now.
- **Memory:** v1 single-shot decrypt holds whole plaintext in RAM; the integration test exercises a multi-MiB encrypted blob as the documented Phase-1 budget.

---

## Task 1: Add chi + sqlc tooling scaffold

**Files:**
- Modify: `go.mod`, `go.sum`, `tools.go`, `Makefile`
- Create: `internal/db/sqlc.yaml`

- [ ] **Step 1.1: Add chi**

```bash
go get github.com/go-chi/chi/v5@latest
```

- [ ] **Step 1.2: Anchor sqlc in tools.go**

Add the import to the block in `tools.go`:

```go
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
```

Then:

```bash
go get github.com/sqlc-dev/sqlc@latest
go mod tidy
```

If `go get` for sqlc fails to resolve as a library dependency, instead pin it via the Makefile using a versioned `go run` (Step 1.4 note) and skip the tools.go anchor; surface the deviation in the commit message.

- [ ] **Step 1.3: Create `internal/db/sqlc.yaml`**

```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "migrations"
    queries: "queries"
    gen:
      go:
        package: "gen"
        out: "gen"
        sql_package: "pgx/v5"
        emit_interface: true
        emit_json_tags: false
        overrides:
          - db_type: "timestamptz"
            go_type: "time.Time"
```

(sqlc recognizes the `-- +goose Up/Down` markers in the migration files and applies only the Up sections for schema inference.)

- [ ] **Step 1.4: Add Makefile targets**

Append to `Makefile` (use TABS for recipe lines):

```makefile
.PHONY: sqlc-generate codegen-check build-coordinator run-coordinator

sqlc-generate:
	cd internal/db && go run github.com/sqlc-dev/sqlc/cmd/sqlc generate

codegen-check: sqlc-generate
	git diff --exit-code -- internal/db/gen || (echo "sqlc drift: run 'make sqlc-generate' and commit" && exit 1)

build-coordinator:
	go build -o bin/coordinator ./cmd/coordinator

run-coordinator:
	go run ./cmd/coordinator
```

- [ ] **Step 1.5: Smoke build**

```bash
go build ./...
```

Expected: builds (no new source consumes chi/sqlc yet).

- [ ] **Step 1.6: Commit**

```bash
git add go.mod go.sum tools.go Makefile internal/db/sqlc.yaml
git commit -s -m "chore(m3): add chi + sqlc tooling scaffold"
```

---

## Task 2: sqlc read queries + generated code

**Files:**
- Create: `internal/db/queries/blobs.sql`, `internal/db/queries/collections.sql`
- Create (generated): `internal/db/gen/*.go`

- [ ] **Step 2.1: Write `internal/db/queries/blobs.sql`**

```sql
-- name: GetBlobCore :one
SELECT
    cid,
    state::text                       AS state,
    mime_type,
    envelope_version,
    (encryption_key_id IS NOT NULL)   AS encrypted,
    COALESCE(owner_id::text, '')      AS owner_id,
    uploaded_at,
    product::text                     AS product
FROM blobs
WHERE cid = $1;

-- name: GetDEKByBlob :one
SELECT
    k.wrapped_key,
    k.state::text                     AS state,
    k.master_key_version_id::text     AS master_key_version_id
FROM blobs b
JOIN data_encryption_keys k ON k.id = b.encryption_key_id
WHERE b.cid = $1;

-- name: GetManifestSize :one
SELECT plaintext_size
FROM blob_manifests
WHERE cid = $1;
```

- [ ] **Step 2.2: Write `internal/db/queries/collections.sql`**

```sql
-- name: ResolveBlobVisibility :many
SELECT c.visibility::text AS visibility
FROM collection_items ci
JOIN collections c ON c.id = ci.collection_id
WHERE ci.blob_cid = $1;
```

- [ ] **Step 2.3: Generate**

```bash
make sqlc-generate
```

Expected: creates `internal/db/gen/db.go`, `models.go`, `blobs.sql.go`, `collections.sql.go`, `querier.go`. Generated row/func shapes:
- `GetBlobCore(ctx, cid string) (GetBlobCoreRow, error)` with fields `Cid string, State string, MimeType string, EnvelopeVersion int16, Encrypted bool, OwnerID string, UploadedAt time.Time, Product string`.
- `GetDEKByBlob(ctx, cid string) (GetDEKByBlobRow, error)` with `WrappedKey []byte, State string, MasterKeyVersionID string`.
- `GetManifestSize(ctx, cid string) (int64, error)`.
- `ResolveBlobVisibility(ctx, blobCid string) ([]string, error)`.

- [ ] **Step 2.4: Verify it compiles**

```bash
go build ./internal/db/...
```

Expected: builds. If sqlc emitted `pgtype.*` for any field above, add a matching `overrides:` entry (e.g. `uuid`→`github.com/google/uuid.UUID`) and regenerate; the `::text`/`COALESCE` casts above are designed to avoid this.

- [ ] **Step 2.5: Commit**

```bash
git add internal/db/queries internal/db/gen
git commit -s -m "feat(m3): sqlc read queries for blob/dek/manifest/visibility"
```

---

## Task 3: storage errors, types, visibility resolver (TDD)

**Files:**
- Create: `pkg/coordinator/storage/errors.go`, `pkg/coordinator/storage/types.go`, `pkg/coordinator/storage/visibility_test.go`

- [ ] **Step 3.1: Write the failing test `visibility_test.go`**

```go
package storage

import "testing"

func TestResolveVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want Visibility
	}{
		{"none", nil, VisibilityPrivate},
		{"only private memberships", []string{"private", "private"}, VisibilityPrivate},
		{"unlisted upgrades", []string{"private", "unlisted"}, VisibilityUnlisted},
		{"public wins", []string{"unlisted", "public", "private"}, VisibilityPublic},
		{"single public", []string{"public"}, VisibilityPublic},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveVisibility(c.in); got != c.want {
				t.Fatalf("resolveVisibility(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 3.2: Run, verify fail**

```bash
go test ./pkg/coordinator/storage/... -run TestResolveVisibility -count=1
```

Expected: FAIL — undefined `Visibility`, `resolveVisibility`.

- [ ] **Step 3.3: Create `errors.go`**

```go
// Package storage is the coordinator's read core. It resolves a blob's
// state and visibility from Postgres, fetches the envelope from the IPFS
// backend, unwraps the per-blob key via the keystore, and decrypts.
//
// The package is HTTP-naïve: it returns the sentinel errors below and the
// internal/api layer maps them to status codes. The same ErrBlobAuthRequired
// maps to 401 on the bytes route and 404 on the .json route.
package storage

import "errors"

var (
	// ErrBlobNotFound: no blobs row for the CID, or the CID is malformed.
	ErrBlobNotFound = errors.New("storage: blob not found")

	// ErrBlobAuthRequired: blob is private (no public/unlisted collection
	// membership). Recoverable via signed URL / bearer in M7 / M6.
	ErrBlobAuthRequired = errors.New("storage: authorization required")

	// ErrBlobQuarantined: blob is under moderation hold.
	ErrBlobQuarantined = errors.New("storage: blob quarantined")

	// ErrBlobSoftDeleted: blob soft-deleted (bytes may still exist).
	ErrBlobSoftDeleted = errors.New("storage: blob soft-deleted")

	// ErrBlobTombstoned: blob tombstoned (key shredded).
	ErrBlobTombstoned = errors.New("storage: blob tombstoned")

	// ErrKeyShredded: encrypted blob whose DEK has been crypto-shredded.
	ErrKeyShredded = errors.New("storage: encryption key shredded")
)
```

- [ ] **Step 3.4: Create `types.go`**

```go
package storage

import (
	"time"

	"github.com/google/uuid"
)

// Visibility is the most-permissive collection visibility a blob has.
// Ordered so a higher value is more permissive.
type Visibility int

const (
	VisibilityPrivate  Visibility = iota // no public/unlisted membership
	VisibilityUnlisted                   // readable anonymously by CID
	VisibilityPublic                     // listed + anonymous
)

func (v Visibility) String() string {
	switch v {
	case VisibilityPublic:
		return "public"
	case VisibilityUnlisted:
		return "unlisted"
	default:
		return "private"
	}
}

// BlobView is the resolved, ready-to-serve description of a blob. The
// exported fields drive headers and JSON metadata; the unexported fields
// carry the key material OpenBytes needs for encrypted blobs.
type BlobView struct {
	CID             string
	MIME            string
	PlaintextSize   int64
	EnvelopeVersion int16
	Product         string
	OwnerID         *string
	UploadedAt      time.Time
	Visibility      Visibility
	Encrypted       bool

	wrappedKey         []byte
	masterKeyVersionID *uuid.UUID
}

// resolveVisibility folds a blob's collection memberships into the single
// most-permissive visibility. No membership ⇒ private.
func resolveVisibility(visibilities []string) Visibility {
	best := VisibilityPrivate
	for _, v := range visibilities {
		switch v {
		case "public":
			return VisibilityPublic
		case "unlisted":
			if best < VisibilityUnlisted {
				best = VisibilityUnlisted
			}
		}
	}
	return best
}
```

- [ ] **Step 3.5: Run, verify pass**

```bash
go test ./pkg/coordinator/storage/... -run TestResolveVisibility -count=1
```

Expected: PASS.

- [ ] **Step 3.6: Commit**

```bash
git add pkg/coordinator/storage/errors.go pkg/coordinator/storage/types.go pkg/coordinator/storage/visibility_test.go
git commit -s -m "feat(m3): storage domain errors, BlobView, visibility resolver"
```

---

## Task 4: storage.Service.Resolve (TDD, Postgres)

**Files:**
- Create: `pkg/coordinator/storage/blob.go`
- Create: `pkg/coordinator/storage/resolve_test.go`

- [ ] **Step 4.1: Write the failing test `resolve_test.go`**

This test inserts rows directly (Resolve does not touch Kubo) and asserts the state/visibility matrix. The `seedRow` helper does raw INSERTs.

```go
package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// seedBlob inserts a blobs row (+ manifest, + optional DEK + collection
// membership) for Resolve tests. It does NOT import to Kubo.
type seedOpts struct {
	cid        string
	state      string // blob_state
	visibility string // "" = no collection membership; else collection visibility
	encrypted  bool
	keyState   string // data_encryption_keys.state when encrypted
}

func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, o seedOpts) {
	t.Helper()
	var ownerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		o.cid+"@example.test").Scan(&ownerID))

	var keyID *uuid.UUID
	if o.encrypted {
		var mkv uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO master_key_versions (version_label, state) VALUES ($1,'active') RETURNING id`,
			"v1-"+o.cid).Scan(&mkv))
		var k uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
			 VALUES ('XChaCha20-Poly1305', $1, $2, $3) RETURNING id`,
			make([]byte, 72), mkv, o.keyState).Scan(&k))
		keyID = &k
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, state, product, envelope_version)
		 VALUES ($1,$2,$3,'application/octet-stream',10,$4,'raw',1)`,
		o.cid, keyID, ownerID, o.state)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256','raw','size-262144',10,58,1)`, o.cid)
	require.NoError(t, err)

	if o.visibility != "" {
		var col uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
			 VALUES ($1,$2,$2,$3,false) RETURNING id`,
			ownerID, "c-"+o.cid, o.visibility).Scan(&col))
		_, err = pool.Exec(ctx,
			`INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`, col, o.cid)
		require.NoError(t, err)
	}
}

const testCID = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"

func cidN(n int) string {
	// distinct valid-looking CIDs per row; the read path only validates
	// the multibase/base32 shape via cid.Decode, so we vary the last char.
	return testCID[:len(testCID)-1] + string(rune('a'+n))
}

func TestResolveMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	svc := storage.NewService(pool, nil, nil) // Resolve needs neither backend nor keystore

	cases := []struct {
		name    string
		o       seedOpts
		wantErr error
		wantVis storage.Visibility
		wantEnc bool
	}{
		{"public encrypted active", seedOpts{cidN(0), "active", "public", true, "active"}, nil, storage.VisibilityPublic, true},
		{"unlisted plaintext active", seedOpts{cidN(1), "active", "unlisted", false, ""}, nil, storage.VisibilityUnlisted, false},
		{"private active", seedOpts{cidN(2), "active", "private", true, "active"}, storage.ErrBlobAuthRequired, 0, false},
		{"no membership", seedOpts{cidN(3), "active", "", true, "active"}, storage.ErrBlobAuthRequired, 0, false},
		{"quarantined", seedOpts{cidN(4), "quarantined", "public", true, "active"}, storage.ErrBlobQuarantined, 0, false},
		{"soft_deleted", seedOpts{cidN(5), "soft_deleted", "public", true, "active"}, storage.ErrBlobSoftDeleted, 0, false},
		{"tombstoned", seedOpts{cidN(6), "tombstoned", "public", true, "shredded"}, storage.ErrBlobTombstoned, 0, false},
		{"key shredded but active", seedOpts{cidN(7), "active", "public", true, "shredded"}, storage.ErrKeyShredded, 0, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			seedBlob(t, ctx, pool, c.o)
			v, err := svc.Resolve(ctx, c.o.cid)
			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.wantVis, v.Visibility)
			require.Equal(t, c.wantEnc, v.Encrypted)
			require.Equal(t, int64(10), v.PlaintextSize)
		})
	}

	t.Run("not found", func(t *testing.T) {
		_, err := svc.Resolve(ctx, cidN(20))
		require.ErrorIs(t, err, storage.ErrBlobNotFound)
	})
	t.Run("bad cid", func(t *testing.T) {
		_, err := svc.Resolve(ctx, "not-a-cid")
		require.ErrorIs(t, err, storage.ErrBlobNotFound)
	})
}
```

- [ ] **Step 4.2: Run, verify fail**

```bash
go test ./pkg/coordinator/storage/... -run TestResolveMatrix -count=1
```

Expected: FAIL — undefined `storage.NewService`, `Resolve`.

- [ ] **Step 4.3: Create `blob.go`**

```go
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// Service is the read core. It is safe for concurrent use.
type Service struct {
	q       *gen.Queries
	backend ipfs.Backend
	ks      *envelope.Keystore
}

// NewService builds a read service over the given pool, IPFS backend, and
// keystore. backend and ks may be nil in tests that exercise Resolve only.
func NewService(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore) *Service {
	return &Service{q: gen.New(pool), backend: backend, ks: ks}
}

// Resolve loads and authorizes a blob for anonymous read. It performs no
// Kubo I/O and no decryption. Returns one of the package sentinels on a
// domain failure, or a wrapped error on infrastructure failure (→ 500).
func (s *Service) Resolve(ctx context.Context, cidStr string) (*BlobView, error) {
	if _, err := cid.Decode(cidStr); err != nil {
		return nil, ErrBlobNotFound
	}

	core, err := s.q.GetBlobCore(ctx, cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get blob core: %w", err)
	}

	switch core.State {
	case "active":
		// continue
	case "quarantined":
		return nil, ErrBlobQuarantined
	case "soft_deleted":
		return nil, ErrBlobSoftDeleted
	case "tombstoned":
		return nil, ErrBlobTombstoned
	default:
		return nil, fmt.Errorf("storage: unexpected blob state %q", core.State)
	}

	vis, err := s.q.ResolveBlobVisibility(ctx, cidStr)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve visibility: %w", err)
	}
	visibility := resolveVisibility(vis)
	if visibility == VisibilityPrivate {
		return nil, ErrBlobAuthRequired
	}

	size, err := s.q.GetManifestSize(ctx, cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("storage: missing manifest for %s", cidStr)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get manifest size: %w", err)
	}

	view := &BlobView{
		CID:             core.Cid,
		MIME:            core.MimeType,
		PlaintextSize:   size,
		EnvelopeVersion: core.EnvelopeVersion,
		Product:         core.Product,
		UploadedAt:      core.UploadedAt,
		Visibility:      visibility,
		Encrypted:       core.Encrypted,
	}
	if core.OwnerID != "" {
		owner := core.OwnerID
		view.OwnerID = &owner
	}

	if core.Encrypted {
		dek, err := s.q.GetDEKByBlob(ctx, cidStr)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("storage: encrypted blob %s has no DEK row", cidStr)
		}
		if err != nil {
			return nil, fmt.Errorf("storage: get dek: %w", err)
		}
		if dek.State == "shredded" {
			return nil, ErrKeyShredded
		}
		mkvID, err := uuid.Parse(dek.MasterKeyVersionID)
		if err != nil {
			return nil, fmt.Errorf("storage: parse master key version id: %w", err)
		}
		view.wrappedKey = dek.WrappedKey
		view.masterKeyVersionID = &mkvID
	}

	return view, nil
}
```

- [ ] **Step 4.4: Run, verify pass**

```bash
go test ./pkg/coordinator/storage/... -run TestResolveMatrix -count=1
```

Expected: PASS (all matrix subtests + not-found + bad-cid).

- [ ] **Step 4.5: Commit**

```bash
git add pkg/coordinator/storage/blob.go pkg/coordinator/storage/resolve_test.go
git commit -s -m "feat(m3): storage.Service.Resolve with state+visibility matrix"
```

---

## Task 5: blob fixture helper (TDD)

A reusable helper that seeds a blob end-to-end (encrypt → import to Kubo → DB rows), used by the OpenBytes test and the integration test. It does raw pgx INSERTs (write queries are M4's domain).

**Files:**
- Create: `internal/blobfixture/blobfixture.go`
- Create: `internal/blobfixture/blobfixture_test.go`

- [ ] **Step 5.1: Write the failing test `blobfixture_test.go`**

```go
package blobfixture_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func newBackend(t *testing.T, ctx context.Context) ipfs.Backend {
	t.Helper()
	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	be, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = be.Close(c)
	})
	return be
}

func TestFixtureEncryptedRoundTrips(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres + kubo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)

	masterHex := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	t.Setenv("NOVA_MASTER_KEY_V1", masterHex)
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	be := newBackend(t, ctx)
	plaintext := []byte("nova fixture plaintext payload")

	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	// Prove the bytes really round-trip from Kubo + decrypt.
	c := res.CID
	rc, err := be.Get(ctx, mustDecodeCID(t, c))
	require.NoError(t, err)
	env, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	_, codec, err := envelope.Decode(env)
	require.NoError(t, err)
	got, err := codec.Decrypt(env, res.PerBlobKey)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, plaintext))

	_ = hex.EncodeToString // keep import if unused after edits
}
```

Add `mustDecodeCID` to the fixture's public API or inline; the simplest is to expose the parsed CID. To avoid a helper here, change the assertion to use `res.ParsedCID` (added below).

- [ ] **Step 5.2: Run, verify fail**

```bash
go test ./internal/blobfixture/... -run TestFixture -count=1
```

Expected: FAIL — package does not exist.

- [ ] **Step 5.3: Create `blobfixture.go`**

```go
// Package blobfixture seeds a fully-formed blob (encrypt → import to Kubo →
// DB rows) for read-path tests. It stands in for the M4 write pipeline.
// Not for production use; it performs raw INSERTs and trusts its inputs.
package blobfixture

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// Deps are the live subsystems the fixture writes through.
type Deps struct {
	Pool     *pgxpool.Pool
	Backend  ipfs.Backend
	Keystore *envelope.Keystore
}

// Spec describes the blob to create.
type Spec struct {
	Plaintext  []byte
	MIME       string
	Visibility string // "public" | "unlisted" | "private" | "" (no membership)
	State      string // blob_state; defaults to "active"
}

// Result reports what was created.
type Result struct {
	CID        string
	ParsedCID  cid.Cid
	PerBlobKey []byte
	OwnerID    uuid.UUID
}

// Seed encrypts the plaintext, imports the envelope to Kubo, and inserts
// users/master_key_versions(if needed)/data_encryption_keys/blobs/
// blob_manifests/blob_blocks/collections/collection_items rows so the read
// path can serve it.
func Seed(ctx context.Context, d Deps, s Spec) (Result, error) {
	if s.State == "" {
		s.State = "active"
	}

	pbk := make([]byte, envelope.KeySize)
	if _, err := rand.Read(pbk); err != nil {
		return Result{}, fmt.Errorf("blobfixture: rand key: %w", err)
	}
	wrapped, mkvID, err := d.Keystore.Wrap(pbk)
	if err != nil {
		return Result{}, fmt.Errorf("blobfixture: wrap: %w", err)
	}
	env, err := envelope.V1().Encrypt(s.Plaintext, pbk)
	if err != nil {
		return Result{}, fmt.Errorf("blobfixture: encrypt: %w", err)
	}
	add, err := d.Backend.AddDeterministic(ctx, env)
	if err != nil {
		return Result{}, fmt.Errorf("blobfixture: import: %w", err)
	}
	cidStr := add.CID.String()

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		cidStr+"@fixture.test").Scan(&ownerID); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert user: %w", err)
	}

	var keyID uuid.UUID
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
		 VALUES ('XChaCha20-Poly1305', $1, $2, 'active') RETURNING id`,
		wrapped, mkvID).Scan(&keyID); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert dek: %w", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, state, product, envelope_version)
		 VALUES ($1,$2,$3,$4,$5,$6,'raw',1)`,
		cidStr, keyID, ownerID, s.MIME, len(s.Plaintext), s.State); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert blob: %w", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256',$2,'size-262144',$3,$4,$5)`,
		cidStr, add.Codec, len(s.Plaintext), add.EnvelopeSize, len(add.Blocks)); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert manifest: %w", err)
	}
	for _, b := range add.Blocks {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
			 VALUES ($1,$2,$3,$4)`, cidStr, b.CID.String(), b.Index, b.Size); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert block: %w", err)
		}
	}

	if s.Visibility != "" {
		var col uuid.UUID
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
			 VALUES ($1,$2,$2,$3,false) RETURNING id`,
			ownerID, "col-"+cidStr[:12], s.Visibility).Scan(&col); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert collection: %w", err)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`,
			col, cidStr); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert collection_item: %w", err)
		}
	}

	return Result{CID: cidStr, ParsedCID: add.CID, PerBlobKey: pbk, OwnerID: ownerID}, nil
}
```

- [ ] **Step 5.4: Fix the test to use `res.ParsedCID`**

Replace `mustDecodeCID(t, c)` usage in Step 5.1 with `res.ParsedCID`, and delete the trailing `_ = hex.EncodeToString` line and the `encoding/hex` import. Final assertion block:

```go
	rc, err := be.Get(ctx, res.ParsedCID)
	require.NoError(t, err)
	env, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	_, codec, err := envelope.Decode(env)
	require.NoError(t, err)
	got, err := codec.Decrypt(env, res.PerBlobKey)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, plaintext))
```

- [ ] **Step 5.5: Run, verify pass**

```bash
go test ./internal/blobfixture/... -run TestFixture -count=1
```

Expected: PASS.

- [ ] **Step 5.6: Commit**

```bash
git add internal/blobfixture
git commit -s -m "test(m3): blobfixture seed helper (encrypt+import+rows)"
```

---

## Task 6: storage.Service.OpenBytes (TDD)

**Files:**
- Modify: `pkg/coordinator/storage/blob.go` (append `OpenBytes`)
- Create: `pkg/coordinator/storage/openbytes_test.go`

- [ ] **Step 6.1: Write the failing test `openbytes_test.go`**

```go
package storage_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

func TestOpenBytesEncrypted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres + kubo")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)

	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	be := newBackend(t, ctx) // from blobfixture_test? define locally — see note
	svc := storage.NewService(pool, be, ks)

	plaintext := []byte("decrypt me through the storage service")
	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	require.True(t, view.Encrypted)

	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}
```

Note: `newBackend` is defined in `blobfixture_test` (package `blobfixture_test`), not visible here. Duplicate a small `newKuboBackend(t, ctx)` helper in this package's `openbytes_test.go` using the same body as Step 5.1's `newBackend`. (Both test files may live in `storage_test`; if so, define the helper once. The fixture test is in `blobfixture_test`, a different package, so the storage test needs its own copy.)

- [ ] **Step 6.2: Run, verify fail**

```bash
go test ./pkg/coordinator/storage/... -run TestOpenBytesEncrypted -count=1
```

Expected: FAIL — undefined `OpenBytes` (and/or helper).

- [ ] **Step 6.3: Append `OpenBytes` to `blob.go`**

Add imports `"bytes"`, `"io"` and the envelope package (already imported). Append:

```go
// OpenBytes returns a reader over the blob's plaintext. For public_archival
// (unencrypted) blobs it streams directly from the backend (Range-friendly
// upstream). For encrypted blobs it fetches the whole envelope, unwraps the
// per-blob key, and decrypts in memory (v1 is single-shot; Phase 2 streaming
// AEAD removes the whole-object buffering). The caller MUST Close the reader.
func (s *Service) OpenBytes(ctx context.Context, v *BlobView) (io.ReadCloser, error) {
	c, err := cid.Decode(v.CID)
	if err != nil {
		return nil, fmt.Errorf("storage: decode cid: %w", err)
	}
	rc, err := s.backend.Get(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("storage: backend get: %w", err)
	}
	if !v.Encrypted {
		return rc, nil
	}
	defer rc.Close()

	env, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("storage: read envelope: %w", err)
	}
	if v.masterKeyVersionID == nil {
		return nil, fmt.Errorf("storage: encrypted view missing key material")
	}
	perBlobKey, err := s.ks.Unwrap(v.wrappedKey, *v.masterKeyVersionID)
	if err != nil {
		return nil, fmt.Errorf("storage: unwrap per-blob key: %w", err)
	}
	_, codec, err := envelope.Decode(env)
	if err != nil {
		return nil, fmt.Errorf("storage: decode envelope: %w", err)
	}
	plain, err := codec.Decrypt(env, perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("storage: decrypt: %w", err)
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
}
```

- [ ] **Step 6.4: Run, verify pass**

```bash
go test ./pkg/coordinator/storage/... -count=1
```

Expected: PASS (Resolve + OpenBytes + visibility).

- [ ] **Step 6.5: Commit**

```bash
git add pkg/coordinator/storage/blob.go pkg/coordinator/storage/openbytes_test.go
git commit -s -m "feat(m3): storage.Service.OpenBytes (stream plaintext / decrypt envelope)"
```

---

## Task 7: API error model (TDD)

**Files:**
- Create: `internal/api/errors.go`, `internal/api/errors_test.go`

- [ ] **Step 7.1: Write the failing test `errors_test.go`**

```go
package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api"
	"github.com/stretchr/testify/require"
)

func TestWriteError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	api.WriteError(rec, 404, "not_found", "blob not found", "req-123")

	require.Equal(t, 404, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "not_found", body["code"])
	require.Equal(t, "blob not found", body["message"])
	require.Equal(t, "req-123", body["request_id"])
}
```

- [ ] **Step 7.2: Run, verify fail**

```bash
go test ./internal/api/ -run TestWriteError -count=1
```

Expected: FAIL — undefined `api.WriteError`.

- [ ] **Step 7.3: Create `errors.go`**

```go
// Package api wires the coordinator's HTTP surface: chi router, middleware,
// and handlers. Handlers translate pkg/coordinator/storage domain errors
// into the JSON Error model from docs/specs/openapi.yaml.
package api

import (
	"encoding/json"
	"net/http"
)

// errorBody is the openapi #/components/schemas/Error shape.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes a JSON Error with the given status. Error responses are
// never cacheable.
func WriteError(w http.ResponseWriter, status int, code, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: code, Message: message, RequestID: requestID})
}
```

- [ ] **Step 7.4: Run, verify pass**

```bash
go test ./internal/api/ -run TestWriteError -count=1
```

Expected: PASS.

- [ ] **Step 7.5: Commit**

```bash
git add internal/api/errors.go internal/api/errors_test.go
git commit -s -m "feat(m3): api JSON Error model + WriteError"
```

---

## Task 8: request-id + recover middleware (TDD)

**Files:**
- Create: `internal/api/middleware/requestid.go`, `internal/api/middleware/recover.go`, `internal/api/middleware/middleware_test.go`

- [ ] **Step 8.1: Write the failing test `middleware_test.go`**

```go
package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/stretchr/testify/require"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	t.Parallel()
	var seen string
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = middleware.RequestIDFromContext(r.Context())
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	require.NotEmpty(t, seen)
	require.Equal(t, seen, rec.Header().Get("X-Request-ID"))
}

func TestRequestIDPropagatesInbound(t *testing.T) {
	t.Parallel()
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "abc-123", middleware.RequestIDFromContext(r.Context()))
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "abc-123")
	h.ServeHTTP(rec, req)
	require.Equal(t, "abc-123", rec.Header().Get("X-Request-ID"))
}

func TestRecoverReturns500(t *testing.T) {
	t.Parallel()
	h := middleware.RequestID(middleware.Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	require.Equal(t, 500, rec.Code)
	require.Contains(t, rec.Body.String(), "internal")
	require.NotContains(t, rec.Body.String(), "boom") // no stack/detail leak
}
```

- [ ] **Step 8.2: Run, verify fail**

```bash
go test ./internal/api/middleware/ -count=1
```

Expected: FAIL — undefined symbols.

- [ ] **Step 8.3: Create `requestid.go`**

```go
// Package middleware holds the coordinator's chi-compatible HTTP middleware.
package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID ensures every request has an X-Request-ID: it honors an inbound
// header (set by nginx) or generates a UUID, stores it in the context, and
// echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request id, or "" if unset.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}
```

- [ ] **Step 8.4: Create `recover.go`**

```go
package middleware

import (
	"log/slog"
	"net/http"

	"github.com/nova-archive/nova/internal/api"
)

// Recover converts a panic in a downstream handler into a 500 JSON Error,
// logging the panic locally without leaking it to the client.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				rid := RequestIDFromContext(r.Context())
				slog.Error("panic in handler", "request_id", rid, "panic", rec, "path", r.URL.Path)
				api.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 8.5: Run, verify pass**

```bash
go test ./internal/api/middleware/ -count=1
```

Expected: PASS.

- [ ] **Step 8.6: Commit**

```bash
git add internal/api/middleware/requestid.go internal/api/middleware/recover.go internal/api/middleware/middleware_test.go
git commit -s -m "feat(m3): request-id + recover middleware"
```

---

## Task 9: rate-limit bucket + middleware (TDD)

**Files:**
- Create: `internal/ratelimit/bucket.go`, `internal/ratelimit/bucket_test.go`
- Create: `internal/api/middleware/ratelimit.go`, `internal/api/middleware/ratelimit_test.go`

- [ ] **Step 9.1: Write the failing test `bucket_test.go`**

```go
package ratelimit_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestBucketAllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 10, Burst: 3}, clock)

	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.False(t, l.Allow("ip-a"), "burst of 3 exhausted")

	require.True(t, l.Allow("ip-b"), "separate key has its own bucket")

	now = now.Add(200 * time.Millisecond) // +2 tokens at 10/s
	require.True(t, l.Allow("ip-a"))
	require.True(t, l.Allow("ip-a"))
	require.False(t, l.Allow("ip-a"))
}
```

- [ ] **Step 9.2: Run, verify fail**

```bash
go test ./internal/ratelimit/ -count=1
```

Expected: FAIL — undefined.

- [ ] **Step 9.3: Create `bucket.go`**

```go
// Package ratelimit provides a per-key token-bucket limiter used as
// in-process defense-in-depth. nginx is the primary limiter in production;
// this guards the coordinator directly.
package ratelimit

import (
	"sync"
	"time"
)

// Config tunes the limiter. RatePerSec is the steady refill; Burst is the
// bucket capacity.
type Config struct {
	RatePerSec float64
	Burst      float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a concurrency-safe keyed token-bucket limiter.
type Limiter struct {
	cfg   Config
	now   func() time.Time
	mu    sync.Mutex
	keys  map[string]*bucket
}

// NewLimiter builds a limiter. clock may be nil (defaults to time.Now);
// tests inject a fixed clock.
func NewLimiter(cfg Config, clock func() time.Time) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{cfg: cfg, now: clock, keys: make(map[string]*bucket)}
}

// Allow reports whether one event for key may proceed, consuming a token.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.keys[key]
	if !ok {
		b = &bucket{tokens: l.cfg.Burst, last: now}
		l.keys[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.cfg.RatePerSec
		if b.tokens > l.cfg.Burst {
			b.tokens = l.cfg.Burst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
```

- [ ] **Step 9.4: Run, verify pass**

```bash
go test ./internal/ratelimit/ -count=1
```

Expected: PASS.

- [ ] **Step 9.5: Write the failing test `ratelimit_test.go`**

```go
package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestRateLimitMiddleware429(t *testing.T) {
	t.Parallel()
	l := ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 0, Burst: 1}, nil)
	h := middleware.RequestID(middleware.RateLimit(l)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})))

	call := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/blob/x", nil)
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, 200, call())
	require.Equal(t, 429, call())
}
```

- [ ] **Step 9.6: Create `ratelimit.go`**

```go
package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// RateLimit returns middleware that rejects requests exceeding the per-IP
// limiter with 429. The client IP is taken from X-Forwarded-For (nginx) and
// falls back to RemoteAddr.
func RateLimit(l *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(clientIP(r)) {
				api.WriteError(w, http.StatusTooManyRequests, "rate_limited",
					"too many requests", RequestIDFromContext(r.Context()))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
```

- [ ] **Step 9.7: Run, verify pass**

```bash
go test ./internal/ratelimit/ ./internal/api/middleware/ -count=1
```

Expected: PASS.

- [ ] **Step 9.8: Commit**

```bash
git add internal/ratelimit internal/api/middleware/ratelimit.go internal/api/middleware/ratelimit_test.go
git commit -s -m "feat(m3): per-IP token-bucket rate-limit middleware"
```

---

## Task 10: health handler (TDD)

**Files:**
- Create: `internal/api/handlers/health.go`, `internal/api/handlers/health_test.go`

- [ ] **Step 10.1: Write the failing test `health_test.go`**

```go
package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/stretchr/testify/require"
)

func TestHealth(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handlers.Health("v0.1.0-test").ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	require.Equal(t, 200, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body["status"])
	require.Equal(t, "v0.1.0-test", body["version"])
	require.NotEmpty(t, body["time"])
}
```

- [ ] **Step 10.2: Run, verify fail**

```bash
go test ./internal/api/handlers/ -run TestHealth -count=1
```

Expected: FAIL — undefined.

- [ ] **Step 10.3: Create `health.go`**

```go
// Package handlers holds the coordinator's HTTP handlers.
package handlers

import (
	"encoding/json"
	"net/http"
	"time"
)

// Health returns a liveness handler. Per openapi.yaml it always returns 200
// when the server is accepting traffic; it does NOT probe DB or Kubo.
func Health(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": version,
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}
```

- [ ] **Step 10.4: Run, verify pass**

```bash
go test ./internal/api/handlers/ -run TestHealth -count=1
```

Expected: PASS.

- [ ] **Step 10.5: Commit**

```bash
git add internal/api/handlers/health.go internal/api/handlers/health_test.go
git commit -s -m "feat(m3): /health liveness handler"
```

---

## Task 11: blob handlers + router (TDD with fake storage)

**Files:**
- Create: `internal/api/handlers/blob.go`, `internal/api/handlers/blob_test.go`
- Create: `internal/api/server.go`, `internal/api/server_test.go`

- [ ] **Step 11.1: Write the failing test `blob_test.go`**

The handler depends on a `Reader` interface; the test fakes it.

```go
package handlers_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

type fakeReader struct {
	view *storage.BlobView
	err  error
	body string
}

func (f fakeReader) Resolve(_ context.Context, _ string) (*storage.BlobView, error) {
	return f.view, f.err
}
func (f fakeReader) OpenBytes(_ context.Context, _ *storage.BlobView) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func route(h *handlers.BlobHandler) chi.Router {
	r := chi.NewRouter()
	r.Get("/blob/{cid}", h.Get)
	r.Head("/blob/{cid}", h.Head)
	r.Get("/blob/{cid}.json", h.JSON)
	return r
}

func TestBlobGetPublicEncrypted(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyX", nil))

	require.Equal(t, 200, rec.Code)
	require.Equal(t, "hello", rec.Body.String())
	require.Equal(t, "text/plain", rec.Header().Get("Content-Type"))
	require.Equal(t, `"bafyX"`, rec.Header().Get("ETag"))
	require.Equal(t, "bafyX", rec.Header().Get("X-Nova-Cid"))
	require.Equal(t, "1", rec.Header().Get("X-Nova-Envelope-Version"))
	require.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
}

func TestBlobGetUnlistedCacheHeader(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyU", MIME: "text/plain", PlaintextSize: 2,
		EnvelopeVersion: 1, Visibility: storage.VisibilityUnlisted, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hi"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyU", nil))
	require.Equal(t, "private, max-age=300, must-revalidate", rec.Header().Get("Cache-Control"))
}

func TestBlobRangeOnEncrypted416(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", Encrypted: true,
		Visibility: storage.VisibilityPublic, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/blob/bafyX", nil)
	req.Header.Set("Range", "bytes=0-1")
	route(h).ServeHTTP(rec, req)
	require.Equal(t, 416, rec.Code)
}

func TestBlobHeadNoBody(t *testing.T) {
	t.Parallel()
	view := &storage.BlobView{CID: "bafyX", MIME: "text/plain", PlaintextSize: 5,
		EnvelopeVersion: 1, Visibility: storage.VisibilityPublic, Encrypted: true, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view, body: "hello"})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("HEAD", "/blob/bafyX", nil))
	require.Equal(t, 200, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, "5", rec.Header().Get("Content-Length"))
}

func TestBlobStatusMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		err       error
		bytesCode int
		jsonCode  int
	}{
		{"not found", storage.ErrBlobNotFound, 404, 404},
		{"auth required", storage.ErrBlobAuthRequired, 401, 404},
		{"quarantined", storage.ErrBlobQuarantined, 451, 404},
		{"soft deleted", storage.ErrBlobSoftDeleted, 410, 410},
		{"tombstoned", storage.ErrBlobTombstoned, 410, 410},
		{"key shredded", storage.ErrKeyShredded, 410, 410},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := handlers.NewBlobHandler(fakeReader{err: c.err})
			rb := httptest.NewRecorder()
			route(h).ServeHTTP(rb, httptest.NewRequest("GET", "/blob/x", nil))
			require.Equal(t, c.bytesCode, rb.Code)
			rj := httptest.NewRecorder()
			route(h).ServeHTTP(rj, httptest.NewRequest("GET", "/blob/x.json", nil))
			require.Equal(t, c.jsonCode, rj.Code)
		})
	}
}

func TestBlobJSONPublic(t *testing.T) {
	t.Parallel()
	owner := "11111111-1111-1111-1111-111111111111"
	view := &storage.BlobView{CID: "bafyX", MIME: "image/png", PlaintextSize: 99,
		Product: "raw", OwnerID: &owner, Visibility: storage.VisibilityPublic, UploadedAt: time.Now()}
	h := handlers.NewBlobHandler(fakeReader{view: view})
	rec := httptest.NewRecorder()
	route(h).ServeHTTP(rec, httptest.NewRequest("GET", "/blob/bafyX.json", nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"cid":"bafyX"`)
	require.Contains(t, rec.Body.String(), `"byte_size":99`)
	require.Contains(t, rec.Body.String(), `"state":"active"`)
}
```

- [ ] **Step 11.2: Run, verify fail**

```bash
go test ./internal/api/handlers/ -run TestBlob -count=1
```

Expected: FAIL — undefined `handlers.BlobHandler`, `NewBlobHandler`.

- [ ] **Step 11.3: Create `blob.go`**

```go
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Reader is the storage read surface the blob handlers depend on.
type Reader interface {
	Resolve(ctx context.Context, cid string) (*storage.BlobView, error)
	OpenBytes(ctx context.Context, v *storage.BlobView) (io.ReadCloser, error)
}

// BlobHandler serves /blob/{cid} (GET, HEAD) and /blob/{cid}.json.
type BlobHandler struct{ r Reader }

// NewBlobHandler builds a blob handler over a storage reader.
func NewBlobHandler(r Reader) *BlobHandler { return &BlobHandler{r: r} }

func cacheControl(v storage.Visibility) string {
	if v == storage.VisibilityPublic {
		return "public, max-age=31536000, immutable"
	}
	return "private, max-age=300, must-revalidate"
}

// mapBytesError maps a storage error to a bytes-route status (GET/HEAD).
func mapBytesError(err error) (status int, code, msg string) {
	switch {
	case errors.Is(err, storage.ErrBlobNotFound):
		return 404, "not_found", "blob not found"
	case errors.Is(err, storage.ErrBlobAuthRequired):
		return 401, "signed_url_required", "signed url or bearer required"
	case errors.Is(err, storage.ErrBlobQuarantined):
		return 451, "quarantined", "content under moderation hold"
	case errors.Is(err, storage.ErrBlobSoftDeleted),
		errors.Is(err, storage.ErrBlobTombstoned),
		errors.Is(err, storage.ErrKeyShredded):
		return 410, "gone", "content no longer available"
	default:
		return 500, "internal", "internal server error"
	}
}

// mapJSONError maps a storage error to a .json-route status. Private and
// quarantined collapse to 404 to avoid leaking existence.
func mapJSONError(err error) (status int, code, msg string) {
	switch {
	case errors.Is(err, storage.ErrBlobNotFound),
		errors.Is(err, storage.ErrBlobAuthRequired),
		errors.Is(err, storage.ErrBlobQuarantined):
		return 404, "not_found", "blob not found"
	case errors.Is(err, storage.ErrBlobSoftDeleted),
		errors.Is(err, storage.ErrBlobTombstoned),
		errors.Is(err, storage.ErrKeyShredded):
		return 410, "gone", "content no longer available"
	default:
		return 500, "internal", "internal server error"
	}
}

func (h *BlobHandler) setBytesHeaders(w http.ResponseWriter, v *storage.BlobView) {
	w.Header().Set("Content-Type", v.MIME)
	w.Header().Set("ETag", `"`+v.CID+`"`)
	w.Header().Set("X-Nova-Cid", v.CID)
	w.Header().Set("X-Nova-Envelope-Version", strconv.Itoa(int(v.EnvelopeVersion)))
	w.Header().Set("Cache-Control", cacheControl(v.Visibility))
}

// Get serves the decrypted bytes.
func (h *BlobHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	v, err := h.r.Resolve(r.Context(), chi.URLParam(r, "cid"))
	if err != nil {
		api.WriteError(w, mapBytesErrorArgs(err, rid))
		return
	}

	hasRange := r.Header.Get("Range") != ""
	if hasRange && v.Encrypted {
		api.WriteError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable",
			"range requests are not supported for encrypted blobs", rid)
		return
	}

	body, err := h.r.OpenBytes(r.Context(), v)
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
		return
	}
	defer body.Close()

	h.setBytesHeaders(w, v)

	if hasRange && !v.Encrypted {
		buf, err := io.ReadAll(body)
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, "internal", "internal server error", rid)
			return
		}
		http.ServeContent(w, r, "", v.UploadedAt, bytes.NewReader(buf))
		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(v.PlaintextSize, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// Head returns headers only; it never fetches or decrypts.
func (h *BlobHandler) Head(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	v, err := h.r.Resolve(r.Context(), chi.URLParam(r, "cid"))
	if err != nil {
		status, _, _ := mapBytesError(err)
		w.WriteHeader(status)
		return
	}
	h.setBytesHeaders(w, v)
	w.Header().Set("Content-Length", strconv.FormatInt(v.PlaintextSize, 10))
	w.WriteHeader(http.StatusOK)
}

// JSON serves public blob metadata.
func (h *BlobHandler) JSON(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	v, err := h.r.Resolve(r.Context(), chi.URLParam(r, "cid"))
	if err != nil {
		api.WriteError(w, mapJSONErrorArgs(err, rid))
		return
	}
	out := map[string]any{
		"cid":         v.CID,
		"mime_type":   v.MIME,
		"byte_size":   v.PlaintextSize,
		"uploaded_at": v.UploadedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"state":       "active",
		"product":     v.Product,
		"urls": map[string]string{
			"bytes": "/blob/" + v.CID,
			"json":  "/blob/" + v.CID + ".json",
		},
	}
	if v.OwnerID != nil {
		out["owner_id"] = *v.OwnerID
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControl(v.Visibility))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// small adapters so WriteError keeps its 4-arg shape
func mapBytesErrorArgs(err error, rid string) (http.ResponseWriter, int, string, string, string) {
	// unreachable shim; replaced below
	panic("use mapBytesError")
}
```

Note: the `mapBytesErrorArgs`/`mapJSONErrorArgs` shims above don't compile cleanly (Go can't spread a tuple into `WriteError`). Replace the two call sites to expand explicitly and **delete** the shim functions:

```go
// in Get:
	if err != nil {
		status, code, msg := mapBytesError(err)
		api.WriteError(w, status, code, msg, rid)
		return
	}
// in JSON:
	if err != nil {
		status, code, msg := mapJSONError(err)
		api.WriteError(w, status, code, msg, rid)
		return
	}
```

Ensure the final file has no `mapBytesErrorArgs`/`mapJSONErrorArgs`.

- [ ] **Step 11.4: Run, verify pass**

```bash
go test ./internal/api/handlers/ -run TestBlob -count=1
```

Expected: PASS. (`http.ServeContent` with a `bytes.Reader` yields 206 for valid ranges on plaintext; that path is covered in the integration test.)

- [ ] **Step 11.5: Write `server_test.go`**

```go
package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/stretchr/testify/require"
)

func TestServerRoutesAndReservedNamespaces(t *testing.T) {
	t.Parallel()
	srv := api.NewServer(api.ServerConfig{
		Version: "test",
		Blob:    handlers.NewBlobHandler(nil), // health-only routes exercised here
		Limiter: ratelimit.NewLimiter(ratelimit.Config{RatePerSec: 1000, Burst: 1000}, nil),
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	require.Equal(t, 200, rec.Code)
	require.NotEmpty(t, rec.Header().Get("X-Request-ID"))

	for _, p := range []string{"/api/v1/anything", "/i/bafyX", "/fed/v1/x", "/v/x", "/a/x", "/d/x", "/r/x"} {
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		require.Equal(t, http.StatusNotFound, rr.Code, "reserved prefix %s must 404 in M3", p)
	}
}
```

- [ ] **Step 11.6: Create `server.go`**

```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/ratelimit"
)

// ServerConfig carries the handlers + knobs the router needs.
type ServerConfig struct {
	Version string
	Blob    *handlers.BlobHandler
	Limiter *ratelimit.Limiter
}

// NewServer assembles the chi router with the M3 middleware stack and the
// read-path routes. Storage-core and product namespaces are reserved (they
// 404 via chi's NotFound until their owning milestones mount them).
func NewServer(cfg ServerConfig) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recover)
	if cfg.Limiter != nil {
		r.Use(middleware.RateLimit(cfg.Limiter))
	}

	r.Get("/health", handlers.Health(cfg.Version))

	if cfg.Blob != nil {
		// Order: register the .json route before the bare {cid} so chi's
		// trailing match doesn't swallow ".json".
		r.Get("/blob/{cid}.json", cfg.Blob.JSON)
		r.Get("/blob/{cid}", cfg.Blob.Get)
		r.Head("/blob/{cid}", cfg.Blob.Head)
	}

	return r
}
```

Note: if `chi` treats `{cid}` greedily and `/blob/abc.json` matches `/blob/{cid}` with `cid="abc.json"`, adjust by routing `/blob/{cid}` and dispatching on a `.json` suffix inside the handler. Validate with the `server_test.go` plus a dedicated test:

```go
func TestBlobJSONRouteDistinctFromBytes(t *testing.T) {
	// add to server_test.go: a fake blob handler is overkill here; assert
	// via handlers test that "/blob/x.json" hits JSON not Get. If chi maps
	// both to {cid}, switch server.go to a single {cid} route that checks
	// strings.HasSuffix(cid, ".json").
}
```

If the suffix collision occurs, replace the three routes with one and branch:

```go
	r.Get("/blob/{cid}", func(w http.ResponseWriter, req *http.Request) {
		if c := chi.URLParam(req, "cid"); strings.HasSuffix(c, ".json") {
			cfg.Blob.JSON(w, req)
			return
		}
		cfg.Blob.Get(w, req)
	})
	r.Head("/blob/{cid}", cfg.Blob.Head)
```

…and in the `.json` handler, trim the suffix from `chi.URLParam(r,"cid")` before calling `Resolve`. Pick whichever the test proves correct; keep the handler tests green.

- [ ] **Step 11.7: Run, verify pass**

```bash
go test ./internal/api/... -count=1
```

Expected: PASS.

- [ ] **Step 11.8: Commit**

```bash
git add internal/api/handlers/blob.go internal/api/handlers/blob_test.go internal/api/server.go internal/api/server_test.go
git commit -s -m "feat(m3): blob GET/HEAD/.json handlers + chi router with reserved namespaces"
```

---

## Task 12: nova_dev anonymous floor (TDD, build-tagged)

**Files:**
- Create: `internal/auth/anonymous_prod.go`, `internal/auth/anonymous_dev.go`, `internal/auth/anonymous_test.go`

- [ ] **Step 12.1: Write the test `anonymous_test.go`** (runs under default = non-dev build)

```go
//go:build !nova_dev

package auth_test

import (
	"testing"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/stretchr/testify/require"
)

func TestAnonymousRefusedInProd(t *testing.T) {
	t.Parallel()
	require.Error(t, auth.EnforceAnonymousPolicy(true), "production build must refuse auth.anonymous=true")
	require.NoError(t, auth.EnforceAnonymousPolicy(false))
}
```

- [ ] **Step 12.2: Run, verify fail**

```bash
go test ./internal/auth/ -count=1
```

Expected: FAIL — undefined `auth.EnforceAnonymousPolicy`.

- [ ] **Step 12.3: Create `anonymous_prod.go`**

```go
//go:build !nova_dev

// Package auth holds the coordinator's authentication surface. M3 ships only
// the anonymous-mode startup floor; bearer (M6) and signed URLs (M7) land
// later. Two build-tagged files implement EnforceAnonymousPolicy: production
// (this file) refuses anonymous mode; the nova_dev build permits it.
package auth

import "errors"

// EnforceAnonymousPolicy returns an error in production builds when the
// operator set auth.anonymous=true. Anonymous management bypass is a dev-only
// affordance; a production binary must refuse to start.
func EnforceAnonymousPolicy(anonymous bool) error {
	if anonymous {
		return errors.New("auth: anonymous mode is not permitted in production builds (rebuild with -tags nova_dev for local dev)")
	}
	return nil
}
```

- [ ] **Step 12.4: Create `anonymous_dev.go`**

```go
//go:build nova_dev

package auth

// EnforceAnonymousPolicy is a no-op in nova_dev builds: anonymous management
// bypass is permitted for local development only. M6 drops nova_dev from
// production builds entirely.
func EnforceAnonymousPolicy(anonymous bool) error { return nil }
```

- [ ] **Step 12.5: Run, verify pass (both build variants)**

```bash
go test ./internal/auth/ -count=1
go build -tags nova_dev ./internal/auth/
```

Expected: default test PASSES; the dev build compiles.

- [ ] **Step 12.6: Commit**

```bash
git add internal/auth/anonymous_prod.go internal/auth/anonymous_dev.go internal/auth/anonymous_test.go
git commit -s -m "feat(m3): nova_dev anonymous startup floor (refuse-to-start in prod)"
```

---

## Task 13: pkg/coordinator lifecycle (TDD)

**Files:**
- Create: `pkg/coordinator/coordinator.go`, `pkg/coordinator/coordinator_test.go`

- [ ] **Step 13.1: Write the failing test `coordinator_test.go`**

```go
package coordinator_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorRunServesHealthAndShutsDown(t *testing.T) {
	t.Parallel()
	c, err := coordinator.New(nil, nil, nil, coordinator.Config{
		ListenAddr: "127.0.0.1:0",
		Version:    "test",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- c.Run(ctx) }()

	var addr string
	require.Eventually(t, func() bool { addr = c.Addr(); return addr != "" }, 3*time.Second, 10*time.Millisecond)

	resp, err := http.Get("http://" + addr + "/health")
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
```

- [ ] **Step 13.2: Run, verify fail**

```bash
go test ./pkg/coordinator/ -count=1
```

Expected: FAIL — undefined.

- [ ] **Step 13.3: Create `coordinator.go`**

```go
// Package coordinator is Nova's public, semver-stable coordinator library.
// It owns the HTTP server and composes the storage read core over injected
// dependencies (a pgx pool, an IPFS backend, and a keystore). Dependency
// construction (env, secrets, Kubo boot) is the caller's responsibility —
// see cmd/coordinator. Product registration arrives in M5.
package coordinator

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/ratelimit"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// RateLimitConfig tunes the in-process per-IP limiter.
type RateLimitConfig struct {
	RatePerSec float64
	Burst      float64
}

// Config holds coordinator settings (not dependencies).
type Config struct {
	ListenAddr string
	Version    string
	RateLimit  RateLimitConfig
}

// Coordinator owns the HTTP server. Build with New; drive with Run/Shutdown.
type Coordinator struct {
	cfg     Config
	handler http.Handler
	srv     *http.Server
	addr    atomic.Value // string
}

// New constructs a coordinator from injected dependencies. pool/backend/ks
// may be nil for tests that only exercise health + lifecycle. When all three
// are present, the blob read routes are mounted.
func New(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, cfg Config) (*Coordinator, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("coordinator: ListenAddr is required")
	}
	limiter := ratelimit.NewLimiter(ratelimit.Config{
		RatePerSec: cfg.RateLimit.RatePerSec, Burst: cfg.RateLimit.Burst,
	}, nil)

	sc := api.ServerConfig{Version: cfg.Version, Limiter: limiter}
	if pool != nil && backend != nil && ks != nil {
		svc := storage.NewService(pool, backend, ks)
		sc.Blob = handlers.NewBlobHandler(svc)
	}
	return &Coordinator{cfg: cfg, handler: api.NewServer(sc)}, nil
}

// Addr returns the actual listen address once Run has bound (useful when
// ListenAddr uses :0). Empty until bound.
func (c *Coordinator) Addr() string {
	if v, ok := c.addr.Load().(string); ok {
		return v
	}
	return ""
}

// Run binds the listener and serves until ctx is cancelled, then drains.
func (c *Coordinator) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", c.cfg.ListenAddr)
	if err != nil {
		return err
	}
	c.addr.Store(ln.Addr().String())
	c.srv = &http.Server{Handler: c.handler, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = c.Shutdown(shutdownCtx)
	}()

	if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the HTTP server. It does NOT close injected
// dependencies — the caller owns their lifecycle.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	if c.srv == nil {
		return nil
	}
	return c.srv.Shutdown(ctx)
}
```

- [ ] **Step 13.4: Run, verify pass**

```bash
go test ./pkg/coordinator/ -count=1
```

Expected: PASS.

- [ ] **Step 13.5: Commit**

```bash
git add pkg/coordinator/coordinator.go pkg/coordinator/coordinator_test.go
git commit -s -m "feat(m3): pkg/coordinator lifecycle (New/Run/Shutdown, injected deps)"
```

---

## Task 14: cmd/coordinator wiring

**Files:**
- Create: `cmd/coordinator/main.go`

No new unit test (covered by the integration test in Task 16). Verify by build + `go vet`.

- [ ] **Step 14.1: Create `main.go`**

```go
// Command coordinator runs the Nova single-node coordinator: it opens the
// database, boots the embedded hardened Kubo backend, bootstraps the
// keystore, enforces the startup floor, and serves the HTTP read path.
//
// Configuration is via environment (M3 subset; the operator.yaml loader and
// the setup wizard arrive in later milestones):
//
//	DATABASE_URL              postgres DSN (required)
//	NOVA_MASTER_KEY_<LABEL>   master key hex; NOVA_MASTER_KEY_ACTIVE selects (required)
//	NOVA_LISTEN_ADDR          coordinator bind addr (default ":9000")
//	NOVA_KUBO_REPO            Kubo repo dir (required)
//	IPFS_SWARM_KEY_FILE       swarm key path (required in private mode)
//	NOVA_AUTH_ANONYMOUS       "true" to request anonymous mode (refused in prod builds)
//	NOVA_VERSION              version string for /health (default "dev")
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "coordinator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- startup floor: anonymous policy (build-tag gated) ---
	anonymous := os.Getenv("NOVA_AUTH_ANONYMOUS") == "true"
	if err := auth.EnforceAnonymousPolicy(anonymous); err != nil {
		return err
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	repo := os.Getenv("NOVA_KUBO_REPO")
	if repo == "" {
		return errors.New("NOVA_KUBO_REPO is required")
	}
	swarm := os.Getenv("IPFS_SWARM_KEY_FILE")
	if swarm == "" {
		return errors.New("IPFS_SWARM_KEY_FILE is required in private mode")
	}
	listen := os.Getenv("NOVA_LISTEN_ADDR")
	if listen == "" {
		listen = ":9000"
	}
	version := os.Getenv("NOVA_VERSION")
	if version == "" {
		version = "dev"
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()

	ks, err := envelope.NewKeystoreFromEnv(pool)
	if err != nil {
		return fmt.Errorf("keystore: %w", err)
	}
	if _, err := ks.Bootstrap(ctx); err != nil {
		return fmt.Errorf("keystore bootstrap: %w", err)
	}

	// NewEmbedded runs ValidateConfig (the Kubo hardening floor) before
	// returning. Online=true so the node can serve; private mode also
	// installs the swarm key (M2).
	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath:     repo,
		Mode:         ipfs.ModePrivate,
		SwarmKeyPath: swarm,
		Online:       true,
	})
	if err != nil {
		return fmt.Errorf("ipfs backend: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15_000_000_000)
		defer cancel()
		_ = backend.Close(shutdownCtx)
	}()

	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr: listen,
		Version:    version,
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 50, Burst: 200},
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}

	fmt.Fprintf(os.Stderr, "coordinator: listening on %s\n", listen)
	return c.Run(ctx)
}
```

- [ ] **Step 14.2: Build + vet**

```bash
go build ./cmd/coordinator/ && go vet ./cmd/coordinator/
```

Expected: builds clean. (Replace the `15_000_000_000` literal with `15 * time.Second` and add the `time` import if you prefer; both compile — the literal avoids an extra import.)

- [ ] **Step 14.3: Commit**

```bash
git add cmd/coordinator/main.go
git commit -s -m "feat(m3): cmd/coordinator wiring + startup floor (env-driven)"
```

---

## Task 15: nginx dev proxy config

**Files:**
- Create: `docker/nginx/nova.dev.conf`

- [ ] **Step 15.1: Create `nova.dev.conf`**

```nginx
# Minimal M3 development proxy — NOT the Phase 1 target topology.
# A single HTTP origin that forwards the read path to the coordinator.
# TLS, dual public/admin origins, proxy_cache, per-IP limits, and config
# templating arrive in M7/M11/M13 (wizard-rendered nova.conf replaces this).
#
# Upstream is overridable via the NOVA_UPSTREAM env (default coordinator:9000)
# using nginx's `envsubst` at container start, or hard-set for tests.

server {
    listen 8080;
    server_name _;

    # Generate a request id if the client didn't supply one; pass it through.
    location / {
        return 404;
    }

    location = /health {
        proxy_pass http://coordinator:9000;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Request-ID      $http_x_request_id;
    }

    location /blob/ {
        proxy_pass http://coordinator:9000;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Request-ID      $http_x_request_id;
    }

    # Reserved for later milestones; explicitly 404 at the edge for now so
    # the route families are stable. (/api/v1, /i, /fed/v1, /v, /a, /d, /r)
}
```

- [ ] **Step 15.2: Commit**

```bash
git add docker/nginx/nova.dev.conf
git commit -s -m "feat(m3): minimal dev nginx proxy for the read path"
```

---

## Task 16: integration test through nginx

**Files:**
- Create: `internal/integration/m3_read_api_test.go`

This is the M3 exit test. It runs the coordinator in-process, starts an nginx testcontainer pointed at it via the host gateway, seeds a blob with `blobfixture`, and fetches through nginx.

- [ ] **Step 16.1: Write the test**

```go
package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestIntegrationM3ReadThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M3 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// --- deps ---
	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)

	swarm := filepath.Join(t.TempDir(), "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(swarm,
		[]byte("/key/swarm/psk/1.0.0/\n/base16/\n"+
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")))
	backend, err := ipfs.NewEmbedded(ctx, ipfs.EmbeddedOptions{
		RepoPath: t.TempDir(), Mode: ipfs.ModePrivate, SwarmKeyPath: swarm, Online: false,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = backend.Close(c)
	})

	// --- coordinator in-process on a fixed host port so nginx can reach it ---
	const coordPort = "19000"
	c, err := coordinator.New(pool, backend, ks, coordinator.Config{
		ListenAddr: "0.0.0.0:" + coordPort,
		Version:    "m3-itest",
		RateLimit:  coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
	})
	require.NoError(t, err)
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go func() { _ = c.Run(runCtx) }()
	require.Eventually(t, func() bool { return c.Addr() != "" }, 5*time.Second, 20*time.Millisecond)

	// --- seed a public encrypted blob ---
	plaintext := []byte("M3 reads this through nginx end to end")
	res, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: plaintext, MIME: "text/plain", Visibility: "public"})
	require.NoError(t, err)

	// --- nginx in front, upstream = host gateway:coordPort ---
	base := startNginx(t, ctx, coordPort)

	// /health through nginx
	hresp, err := http.Get(base + "/health")
	require.NoError(t, err)
	require.Equal(t, 200, hresp.StatusCode)
	_ = hresp.Body.Close()

	// /blob/{cid} through nginx → byte-equal
	bresp, err := http.Get(base + "/blob/" + res.CID)
	require.NoError(t, err)
	defer bresp.Body.Close()
	require.Equal(t, 200, bresp.StatusCode)
	got, err := io.ReadAll(bresp.Body)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
	require.Equal(t, "text/plain", bresp.Header.Get("Content-Type"))
	require.Equal(t, `"`+res.CID+`"`, bresp.Header.Get("ETag"))

	// negatives
	requireStatus(t, base+"/blob/not-a-cid", 404)
	// range on encrypted → 416
	req, _ := http.NewRequest("GET", base+"/blob/"+res.CID, nil)
	req.Header.Set("Range", "bytes=0-3")
	rr, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rr.StatusCode)
	_ = rr.Body.Close()

	// private blob → 401 bytes, 404 .json
	pv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: backend, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("secret"), MIME: "text/plain", Visibility: "private"})
	require.NoError(t, err)
	requireStatus(t, base+"/blob/"+pv.CID, 401)
	requireStatus(t, base+"/blob/"+pv.CID+".json", 404)
	requireStatus(t, base+"/blob/"+res.CID+".json", 200)
}

func requireStatus(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, want, resp.StatusCode, "GET %s", url)
}

// startNginx launches an nginx container whose config proxies to the
// in-process coordinator on the host. Returns the base URL (http://host:port).
func startNginx(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	// Render a config that points at the host gateway. testcontainers exposes
	// the host as "host.testcontainers.internal" when HostAccessPorts is set.
	conf := fmt.Sprintf(`
server {
  listen 8080;
  location = /health { proxy_pass http://host.testcontainers.internal:%s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/    { proxy_pass http://host.testcontainers.internal:%s; proxy_set_header X-Forwarded-For $remote_addr; }
}
`, coordPort, coordPort)

	confPath := filepath.Join(t.TempDir(), "default.conf")
	require.NoError(t, ipfs.WriteFileForTest(confPath, []byte(conf)))

	req := testcontainers.ContainerRequest{
		Image:           "nginx:1.25-alpine",
		ExposedPorts:    []string{"8080/tcp"},
		HostAccessPorts: []string{atoiPort(t, coordPort)},
		WaitingFor:      wait.ForListeningPort("8080/tcp").WithStartupTimeout(60 * time.Second),
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      confPath,
			ContainerFilePath: "/etc/nginx/conf.d/default.conf",
			FileMode:          0o644,
		}},
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = ctr.Terminate(c)
	})

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mapped, err := ctr.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}

func atoiPort(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		require.True(t, r >= '0' && r <= '9')
		n = n*10 + int(r-'0')
	}
	return n
}
```

- [ ] **Step 16.2: Run the integration test**

```bash
go test ./internal/integration/ -run TestIntegrationM3ReadThroughNginx -count=1
```

Expected: PASS.

**Fallback** (if `host.testcontainers.internal` / `HostAccessPorts` proves unreliable in this environment): drop the nginx container and point the assertions directly at `"http://" + c.Addr()`; keep a `t.Log` noting nginx is exercised via `make smoke` instead. Record which path was used in the commit message. Do NOT block the milestone on container networking quirks.

- [ ] **Step 16.3: Commit**

```bash
git add internal/integration/m3_read_api_test.go
git commit -s -m "test(m3): integration read path through nginx (exit test)"
```

---

## Task 17: CI gate, master-plan status, full pass

**Files:**
- Modify: `.github/workflows/ci.yml`, `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md`

- [ ] **Step 17.1: Add `codegen-check` to CI**

In `.github/workflows/ci.yml`, in the test/lint job (after `go` is set up), add a step:

```yaml
      - name: sqlc codegen drift
        run: make codegen-check
```

(If the lint job and test job are separate, add it to whichever already checks out code and sets up Go; it needs the toolchain to `go run` sqlc.)

- [ ] **Step 17.2: Update the master plan**

In `docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md`:

- In the milestone table, change the M3 row to:
  `| M3 | Storage core API (read path) | in progress | [m3 plan](2026-05-28-phase1-m3-storage-read-api.md) |`
- In the M3 milestone summary's deliverables, replace the `audit-log` middleware mention with a note: `audit_log middleware is deferred to M6 (first privileged endpoints); M3 ships request-id, recover, and rate-limit only.`

(Exact current wording: open the file, find the M3 table row `| M3 | Storage core API (read path) | pending | tbd |` and the M3 deliverables line listing middleware `(request-id, recover, rate-limit, audit-log)`; edit both.)

- [ ] **Step 17.3: Full test pass**

```bash
go build ./...
go test ./... -count=1            # unit tests (integration skipped under -short? no — runs them)
go vet ./...
make codegen-check
```

Expected: all green. If integration tests are slow, run unit-only with `go test -short ./...` first, then the integration suite explicitly. Investigate any failure before proceeding — do not mark the milestone done with red tests (superpowers:verification-before-completion).

- [ ] **Step 17.4: Commit**

```bash
git add .github/workflows/ci.yml docs/superpowers/plans/phase1/2026-05-25-phase1-single-node-mvp.md
git commit -s -m "ci(m3): sqlc codegen-check gate; mark M3 in progress + reconcile audit_log"
```

---

## Self-review

**Spec coverage:**
- `/health` → Task 10. `/blob/{cid}` GET/HEAD → Task 11. `/blob/{cid}.json` → Task 11. ✓
- storage read core (Resolve + OpenBytes, typed errors) → Tasks 3–6. ✓
- sqlc + committed gen + CI gate → Tasks 1, 2, 17. ✓
- middleware request-id/recover/rate-limit → Tasks 8, 9. ✓
- nova_dev floor → Task 12. ✓
- pkg/coordinator lifecycle (DI) → Task 13; cmd wiring → Task 14. ✓
- dev nginx + nginx-fronted integration test → Tasks 15, 16. ✓
- visibility matrix (unlisted anonymous, private→401 bytes / 404 json), state mapping (451/410), range→416 → Tasks 4, 11, 16. ✓
- memory/size budget → exercised by Task 16 (multi-line payload; extend to a few MiB if desired) and the OpenBytes test. NOTE: bump the integration plaintext to a multi-MiB buffer if a larger budget assertion is wanted.
- namespace reservation → Task 11 (`server_test.go`). ✓
- master-plan reconciliation (audit_log→M6) → Task 17. ✓ (openapi reconciliations already committed on main.)

**Placeholder scan:** Task 11 intentionally flags a possible chi route-collision and gives the exact remedy + a verifying test; Task 5/6 note a test-helper duplication and how to resolve it; Task 16 documents a concrete fallback. These are decision points with complete instructions, not unfilled placeholders.

**Type consistency:** `storage.NewService(pool, backend, ks)`, `Service.Resolve`, `Service.OpenBytes`, `BlobView` fields, and `handlers.Reader` match across Tasks 3/4/6/11/13. The `gen.*` row field names in Task 2 match their use in Task 4. `RateLimitConfig`/`Config` match across Tasks 13/14/16. `auth.EnforceAnonymousPolicy(bool) error` matches across Tasks 12/14.

**Known follow-ups (not M3):** `LIBP2P_FORCE_PNET=1` defense-in-depth when Online (TODO left in `ipfs/embedded.go` by M2) — wire in cmd/coordinator as a small hardening add; if trivial, include in Task 14, else defer to M-hardening. `oapi-codegen` + getBlobJson 451 reconciliation → M4.

---

## Execution

Per the established milestone workflow (and Bug's standing instruction to proceed without per-task approval to conserve usage): execute **subagent-driven** — a fresh subagent per task, with spec/code review between tasks — on branch `m3-storage-read-api`. On completion: full test pass, then local fast-forward merge to `main` + annotated tag `m3-storage-read-api`; no remote push (ask first).
