package handlers_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/auth"
)

// TestIntegrationSigningAdminAuditBackfill verifies the M9 backfill: when an
// audit writer is wired, RotateSigning emits a best-effort audit_log row that
// records the authenticated operator as the actor.
func TestIntegrationSigningAdminAuditBackfill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	h, q, pool := newSigningAdminFixture(t, ctx)
	h.SetAuditWriter(auditlog.NewWriter(q, slog.Default()))

	var op uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		"rot-"+uuid.NewString()+"@audit.test").Scan(&op))
	rctx := auth.ContextWithIdentity(ctx, auth.Identity{UserID: op.String(), Role: "operator"})

	rec := httptest.NewRecorder()
	h.RotateSigning(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)).WithContext(rctx))
	require.Equal(t, http.StatusCreated, rec.Code)

	var n int
	var actor string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(actor_id::text),'') FROM audit_log WHERE action='signing_key.rotated'`).
		Scan(&n, &actor))
	require.Equal(t, 1, n, "rotate writes one best-effort audit_log row")
	require.Equal(t, op.String(), actor, "the audit row records the operator actor")
}
