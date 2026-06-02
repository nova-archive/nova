package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func seedAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid, kind, result string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO integrity_audits (cid, audit_kind, result) VALUES ($1,$2,$3)`,
		cid, kind, result)
	require.NoError(t, err)
}

type auditListResp struct {
	Data       []map[string]any `json:"data"`
	Pagination struct {
		Page    int `json:"page"`
		PerPage int `json:"per_page"`
		Total   int `json:"total"`
	} `json:"pagination"`
}

func TestAuditAdminList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping audits admin DB test in short mode")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedAudit(t, ctx, pool, "bafy1", "kubo_pin_present", "pass")
	seedAudit(t, ctx, pool, "bafy2", "kubo_pin_present", "fail")
	seedAudit(t, ctx, pool, "bafy3", "envelope_decode", "pass")

	h := handlers.NewAuditAdminHandler(gen.New(pool))
	do := func(query string) auditListResp {
		t.Helper()
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest("GET", "/api/v1/admin/audits/integrity"+query, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var out auditListResp
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	all := do("")
	require.Equal(t, 3, all.Pagination.Total)
	require.Len(t, all.Data, 3)
	require.Equal(t, 50, all.Pagination.PerPage)

	fails := do("?result=fail")
	require.Equal(t, 1, fails.Pagination.Total)
	require.Len(t, fails.Data, 1)
	require.Equal(t, "bafy2", fails.Data[0]["cid"])
	require.Equal(t, "kubo_pin_present", fails.Data[0]["audit_kind"])

	pins := do("?audit_kind=kubo_pin_present")
	require.Equal(t, 2, pins.Pagination.Total)
	require.Len(t, pins.Data, 2)

	paged := do("?per_page=1&page=1")
	require.Equal(t, 3, paged.Pagination.Total) // total ignores the page window
	require.Len(t, paged.Data, 1)
	require.Equal(t, 1, paged.Pagination.PerPage)

	// Invalid filters and pagination ⇒ 400.
	for _, q := range []string{"?result=bogus", "?audit_kind=nope", "?page=0"} {
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest("GET", "/api/v1/admin/audits/integrity"+q, nil))
		require.Equal(t, 400, rec.Code, q)
	}
}
