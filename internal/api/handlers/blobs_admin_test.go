package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func TestBlobsAdminListFilterPaginate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	o1 := seedUserRole(t, ctx, pool, "uploader")
	o2 := seedUserRole(t, ctx, pool, "uploader")
	seedAdminBlob(t, ctx, pool, "bafyA", &o1, "active", "image")
	seedAdminBlob(t, ctx, pool, "bafyB", &o1, "soft_deleted", "image")
	seedAdminBlob(t, ctx, pool, "bafyC", &o2, "active", "raw")

	h := handlers.NewBlobsAdminHandler(gen.New(pool))
	do := func(query string) pageResp {
		t.Helper()
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/blobs"+query, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return decodePage(t, rec.Body)
	}

	all := do("")
	require.Equal(t, 3, all.Pagination.Total)
	require.Len(t, all.Data, 3)
	require.Equal(t, 50, all.Pagination.PerPage)

	sd := do("?state=soft_deleted")
	require.Equal(t, 1, sd.Pagination.Total)
	require.Equal(t, "bafyB", sd.Data[0]["cid"])

	raw := do("?product=raw")
	require.Equal(t, 1, raw.Pagination.Total)
	require.Equal(t, "bafyC", raw.Data[0]["cid"])

	byOwner := do("?owner_id=" + o2.String())
	require.Equal(t, 1, byOwner.Pagination.Total)
	require.Equal(t, "bafyC", byOwner.Data[0]["cid"])

	paged := do("?per_page=2&page=1")
	require.Equal(t, 3, paged.Pagination.Total) // total ignores the page window
	require.Len(t, paged.Data, 2)
	require.Equal(t, 2, paged.Pagination.PerPage)

	// Invalid filters / pagination ⇒ 400.
	for _, q := range []string{"?state=bogus", "?product=nope", "?owner_id=not-a-uuid", "?page=0"} {
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/blobs"+q, nil))
		require.Equal(t, 400, rec.Code, q)
	}
}
