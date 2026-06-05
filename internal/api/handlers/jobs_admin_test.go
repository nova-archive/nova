package handlers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/jobs"
)

func seedJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind, state, lastErr string) uuid.UUID {
	t.Helper()
	var le *string
	if lastErr != "" {
		le = &lastErr
	}
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO jobs (kind, payload, state, attempts, max_attempts, last_error)
		 VALUES ($1, '{}'::jsonb, $2::job_state, 0, 5, $3) RETURNING id`,
		kind, state, le).Scan(&id))
	return id
}

func TestJobsAdminListFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	seedJob(t, ctx, pool, "derivative_prewarm", "pending", "")
	seedJob(t, ctx, pool, "derivative_prewarm", "dead", "kubo pin timeout")
	seedJob(t, ctx, pool, "webhook_emit", "completed", "")

	h := handlers.NewJobsAdminHandler(jobs.NewAdminStore(pool))
	do := func(query string) pageResp {
		t.Helper()
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/jobs"+query, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return decodePage(t, rec.Body)
	}

	all := do("")
	require.Equal(t, 3, all.Pagination.Total)
	require.Len(t, all.Data, 3)

	dead := do("?state=dead")
	require.Equal(t, 1, dead.Pagination.Total)
	require.Equal(t, "kubo pin timeout", dead.Data[0]["last_error"])
	require.Equal(t, "dead", dead.Data[0]["state"])

	wh := do("?kind=webhook_emit")
	require.Equal(t, 1, wh.Pagination.Total)
	require.Equal(t, "webhook_emit", wh.Data[0]["kind"])

	// Unknown state ⇒ 400 (the handler validates before the store).
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/jobs?state=bogus", nil))
	require.Equal(t, 400, rec.Code)
}
