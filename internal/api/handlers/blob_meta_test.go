package handlers_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/lifecycle"
)

// --- shared M11 admin/owner-blob test helpers --------------------------------

func seedUserRole(t *testing.T, ctx context.Context, pool *pgxpool.Pool, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,$2) RETURNING id`,
		uuid.NewString()+"@t.test", role).Scan(&id))
	return id
}

// seedAdminBlob inserts a minimal blobs row (no DEK/manifest needed for the
// metadata + soft-delete paths). owner may be nil for an ownerless blob.
func seedAdminBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string, owner *uuid.UUID, state, product string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO blobs (cid, owner_id, mime_type, byte_size, state, product)
		 VALUES ($1,$2,'image/png',123,$3::blob_state,$4::blob_product)`,
		cidStr, owner, state, product)
	require.NoError(t, err)
}

func blobReq(method, cidStr string, id auth.Identity) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/blobs/"+cidStr, nil)
	r = r.WithContext(auth.ContextWithIdentity(r.Context(), id))
	return withURLParam(r, "cid", cidStr)
}

func newBlobMeta(pool *pgxpool.Pool) *handlers.BlobMetaHandler {
	life := lifecycle.NewService(gen.New(pool), pool, nil, nil,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), time.Now, time.Hour)
	return handlers.NewBlobMetaHandler(gen.New(pool), life)
}

func blobAuditCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action, cidStr string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action=$1 AND target_type='cid' AND target_id=$2`,
		action, cidStr).Scan(&n))
	return n
}

// --- GET /api/v1/blobs/{cid} -------------------------------------------------

func TestBlobMetaGetAuthz(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	owner := seedUserRole(t, ctx, pool, "uploader")
	op := seedUserRole(t, ctx, pool, "operator")
	mod := seedUserRole(t, ctx, pool, "moderator")
	other := seedUserRole(t, ctx, pool, "uploader")
	seedAdminBlob(t, ctx, pool, "bafyMetaGet", &owner, "active", "image")

	h := newBlobMeta(pool)
	code := func(id auth.Identity) int {
		rec := httptest.NewRecorder()
		h.Get(rec, blobReq(http.MethodGet, "bafyMetaGet", id))
		return rec.Code
	}
	require.Equal(t, 200, code(auth.Identity{UserID: owner.String(), Role: "uploader"}), "owner reads")
	require.Equal(t, 200, code(auth.Identity{UserID: op.String(), Role: "operator"}), "operator reads")
	require.Equal(t, 200, code(auth.Identity{UserID: mod.String(), Role: "moderator"}), "moderator reads")
	require.Equal(t, 403, code(auth.Identity{UserID: other.String(), Role: "uploader"}), "non-owner uploader forbidden")

	rec := httptest.NewRecorder()
	h.Get(rec, blobReq(http.MethodGet, "bafyUnknown", auth.Identity{UserID: op.String(), Role: "operator"}))
	require.Equal(t, 404, rec.Code, "unknown cid")
}

// --- DELETE /api/v1/blobs/{cid} ----------------------------------------------

func TestBlobMetaDeleteSoftDeletesAndAudits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	owner := seedUserRole(t, ctx, pool, "uploader")
	seedAdminBlob(t, ctx, pool, "bafyMetaDel", &owner, "active", "image")

	h := newBlobMeta(pool)
	rec := httptest.NewRecorder()
	h.Delete(rec, blobReq(http.MethodDelete, "bafyMetaDel", auth.Identity{UserID: owner.String(), Role: "uploader"}))
	require.Equal(t, 204, rec.Code, rec.Body.String())
	require.Equal(t, "soft_deleted", modBlobState(t, ctx, pool, "bafyMetaDel"))
	require.Equal(t, 1, blobAuditCount(t, ctx, pool, "blob.soft_deleted", "bafyMetaDel"))

	// A second delete on an already-soft-deleted blob is a conflict.
	rec = httptest.NewRecorder()
	h.Delete(rec, blobReq(http.MethodDelete, "bafyMetaDel", auth.Identity{UserID: owner.String(), Role: "uploader"}))
	require.Equal(t, 409, rec.Code)
}

func TestBlobMetaDeleteAuthz(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	owner := seedUserRole(t, ctx, pool, "uploader")
	mod := seedUserRole(t, ctx, pool, "moderator")
	other := seedUserRole(t, ctx, pool, "uploader")
	op := seedUserRole(t, ctx, pool, "operator")
	seedAdminBlob(t, ctx, pool, "bafyDelAuthz", &owner, "active", "image")

	h := newBlobMeta(pool)
	del := func(id auth.Identity) int {
		rec := httptest.NewRecorder()
		h.Delete(rec, blobReq(http.MethodDelete, "bafyDelAuthz", id))
		return rec.Code
	}
	require.Equal(t, 403, del(auth.Identity{UserID: mod.String(), Role: "moderator"}), "moderator may read but not delete")
	require.Equal(t, 403, del(auth.Identity{UserID: other.String(), Role: "uploader"}), "non-owner forbidden")
	require.Equal(t, "active", modBlobState(t, ctx, pool, "bafyDelAuthz"), "still active after forbidden deletes")
	require.Equal(t, 204, del(auth.Identity{UserID: op.String(), Role: "operator"}), "operator deletes any blob")
}
