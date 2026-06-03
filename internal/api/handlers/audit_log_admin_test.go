package handlers_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
)

func TestAuditLogAdminList(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	op := modOperator(t, ctx, pool)
	h := handlers.NewAuditLogAdminHandler(q)

	// An operator action (actor_id set, JSON payload) and a system action
	// (actor_id NULL — the scheduled-tombstone sweep path).
	require.NoError(t, q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
		ActorID: pgtype.UUID{Bytes: op, Valid: true}, Action: "dmca.tombstoned",
		TargetType: "cid", TargetID: "bafyaudit1", Payload: []byte(`{"reason":"notice"}`),
	}))
	require.NoError(t, q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
		ActorID: pgtype.UUID{}, Action: "dmca.tombstoned",
		TargetType: "cid", TargetID: "bafyaudit2", Payload: []byte(`{"reason":"sweep"}`),
	}))
	require.NoError(t, q.InsertAuditLog(ctx, gen.InsertAuditLogParams{
		ActorID: pgtype.UUID{Bytes: op, Valid: true}, Action: "blocklist.added",
		TargetType: "cid", TargetID: "bafyaudit3", Payload: []byte(`{"reason":"abuse"}`),
	}))

	do := func(query string) pageResp {
		t.Helper()
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest("GET", "/api/v1/admin/audit-log"+query, nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return decodePage(t, rec.Body)
	}

	all := do("")
	require.Equal(t, 3, all.Pagination.Total)
	require.Len(t, all.Data, 3)
	require.Equal(t, 50, all.Pagination.PerPage)

	// actor_id is the operator on a user action; null on the system action.
	// payload round-trips as a JSON object.
	var sawNullActor, sawOpActor bool
	for _, row := range all.Data {
		switch row["target_id"] {
		case "bafyaudit2":
			require.Nil(t, row["actor_id"], "system action ⇒ null actor_id")
			sawNullActor = true
		case "bafyaudit1":
			require.Equal(t, op.String(), row["actor_id"])
			sawOpActor = true
			payload, ok := row["payload"].(map[string]any)
			require.True(t, ok, "payload decodes as a JSON object")
			require.Equal(t, "notice", payload["reason"])
		}
	}
	require.True(t, sawNullActor)
	require.True(t, sawOpActor)

	// Filter by action.
	dmca := do("?action=dmca.tombstoned")
	require.Equal(t, 2, dmca.Pagination.Total)
	require.Len(t, dmca.Data, 2)

	// Filter by target_type (all three share 'cid').
	cids := do("?target_type=cid")
	require.Equal(t, 3, cids.Pagination.Total)

	// Combined filter narrows to one.
	combined := do("?action=blocklist.added&target_type=cid")
	require.Equal(t, 1, combined.Pagination.Total)
	require.Equal(t, "bafyaudit3", combined.Data[0]["target_id"])

	// Pagination window: total ignores the page size.
	paged := do("?per_page=1&page=1")
	require.Equal(t, 3, paged.Pagination.Total)
	require.Len(t, paged.Data, 1)

	// Invalid pagination ⇒ 400.
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest("GET", "/api/v1/admin/audit-log?page=0", nil))
	require.Equal(t, 400, rec.Code)
}
