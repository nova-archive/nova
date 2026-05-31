package localissuer

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func seedUser(t *testing.T, ctx context.Context, q *gen.Queries) uuid.UUID {
	t.Helper()
	u, err := q.CreateUser(ctx, gen.CreateUserParams{
		Email: uuid.NewString() + "@example.com",
		Role:  gen.UserRole("operator"),
	})
	require.NoError(t, err)
	return uuid.UUID(u.ID.Bytes)
}

func TestRefreshRotateIssuesNewAndInvalidatesOld(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)
	rs := newRefreshStore(q, time.Hour)

	raw1, err := rs.issue(ctx, uid, "")
	require.NoError(t, err)

	gotUID, raw2, err := rs.rotate(ctx, raw1, "")
	require.NoError(t, err)
	require.Equal(t, uid, gotUID)
	require.NotEqual(t, raw1, raw2)

	_, _, err = rs.rotate(ctx, raw1, "") // reuse of rotated token
	require.ErrorIs(t, err, errRefreshInvalid)

	_, _, err = rs.rotate(ctx, raw2, "") // family killed
	require.ErrorIs(t, err, errRefreshInvalid)
}

func TestRefreshRevokeThenRotateFails(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)
	rs := newRefreshStore(q, time.Hour)

	raw, err := rs.issue(ctx, uid, "")
	require.NoError(t, err)
	require.NoError(t, rs.revoke(ctx, raw))
	_, _, err = rs.rotate(ctx, raw, "")
	require.ErrorIs(t, err, errRefreshInvalid)
}

func TestRefreshRejectsExpired(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)
	rs := newRefreshStore(q, -time.Minute) // expired on issue

	raw, err := rs.issue(ctx, uid, "")
	require.NoError(t, err)
	_, _, err = rs.rotate(ctx, raw, "")
	require.ErrorIs(t, err, errRefreshInvalid)
}
