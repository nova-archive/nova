package localissuer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// pgTimestamptz converts a time.Time to a valid pgtype.Timestamptz. Test-only
// helper for constructing timestamptz query parameters.
func pgTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

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

// TestRefreshConcurrentRotateSingleWinner issues a token and then fires
// multiple concurrent rotate() calls with the same raw token. Exactly one must
// succeed; all others must receive errRefreshInvalid — verifying that the
// conditional UPDATE in MarkRefreshTokenRotated closes the TOCTOU window.
func TestRefreshConcurrentRotateSingleWinner(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)
	rs := newRefreshStore(q, time.Hour)

	raw, err := rs.issue(ctx, uid, "")
	require.NoError(t, err)

	const goroutines = 4
	start := make(chan struct{})
	type result struct {
		err error
	}
	results := make([]result, goroutines)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // all goroutines start as simultaneously as possible
			_, _, rotErr := rs.rotate(ctx, raw, "")
			mu.Lock()
			results[i] = result{err: rotErr}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, r := range results {
		if r.err == nil {
			winners++
		} else {
			require.ErrorIs(t, r.err, errRefreshInvalid, "unexpected error type from losing goroutine")
		}
	}
	require.Equal(t, 1, winners, "exactly one concurrent rotate should succeed")
}

// TestRefreshDisabledUserCannotRotate seeds a user, issues a token, disables
// the user via a direct SQL exec, and asserts that rotate returns
// errRefreshInvalid — confirming the disabled flag terminates refresh.
func TestRefreshDisabledUserCannotRotate(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)
	rs := newRefreshStore(q, time.Hour)

	raw, err := rs.issue(ctx, uid, "")
	require.NoError(t, err)

	// Disable the user directly in the DB.
	_, err = pool.Exec(ctx, "UPDATE users SET disabled = true WHERE id = $1", pgUUID(uid))
	require.NoError(t, err)

	_, _, err = rs.rotate(ctx, raw, "")
	require.ErrorIs(t, err, errRefreshInvalid)
}

// TestRefreshTokenGCQueriesUseExpectedPredicates verifies the M6.2 C1
// contract for the split GC queries:
//
//   - DeleteExpiredRefreshTokens drops rows where expires_at < now() AND
//     revoked_at IS NULL (matches refresh_tokens_gc_idx partial index).
//   - DeleteRevokedRefreshTokensOlderThan drops rows where revoked_at IS
//     NOT NULL AND revoked_at < cutoff (matches refresh_tokens_revoked_gc_idx).
//
// Together they keep the table tidy; neither query touches rows the other
// owns.
func TestRefreshTokenGCQueriesUseExpectedPredicates(t *testing.T) {
	pool := dbtest.New(t, context.Background())
	ctx := context.Background()
	q := gen.New(pool)
	uid := seedUser(t, ctx, q)

	now := time.Now().UTC()
	// (label, expires_at, revoked_at) — revoked_at NULL means "active".
	rows := []struct {
		label     string
		expiresAt time.Time
		revokedAt *time.Time
	}{
		{"expired-active", now.Add(-2 * time.Hour), nil},
		{"expired-revoked-recent", now.Add(-2 * time.Hour), ptrTime(now.Add(-2 * time.Hour))},
		{"fresh-active", now.Add(time.Hour), nil},
		{"fresh-revoked-recent", now.Add(time.Hour), ptrTime(now.Add(-1 * time.Hour))},
		{"fresh-revoked-old", now.Add(time.Hour), ptrTime(now.Add(-40 * 24 * time.Hour))},
	}
	for _, r := range rows {
		_, err := pool.Exec(ctx,
			`INSERT INTO refresh_tokens (user_id, token_hash, expires_at, revoked_at, user_agent)
			 VALUES ($1, $2, $3, $4, $5)`,
			pgUUID(uid), []byte(r.label), r.expiresAt, r.revokedAt, r.label)
		require.NoError(t, err)
	}

	countLabels := func(t *testing.T) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		rows, err := pool.Query(ctx, `SELECT user_agent FROM refresh_tokens WHERE user_id = $1`, pgUUID(uid))
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var ua string
			require.NoError(t, rows.Scan(&ua))
			got[ua] = true
		}
		return got
	}

	// 1. Expired-but-active GC. Only the expired-active row matches the
	//    `expires_at < now() AND revoked_at IS NULL` predicate — confirming
	//    the partial-index alignment (B6.2 C1). The expired-revoked-recent
	//    row stays put even though it's expired, because its cleanup is
	//    owned by the revoked-GC below.
	n, err := q.DeleteExpiredRefreshTokens(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "exactly the expired-active row is deleted")

	after1 := countLabels(t)
	require.False(t, after1["expired-active"], "expired-active dropped")
	require.True(t, after1["expired-revoked-recent"], "expired-revoked-recent NOT deleted by GC1 (revoked filter excludes it)")
	require.True(t, after1["fresh-active"], "fresh-active untouched")
	require.True(t, after1["fresh-revoked-recent"], "fresh-revoked-recent within grace")
	require.True(t, after1["fresh-revoked-old"], "fresh-revoked-old not yet touched by expired-active GC")

	// 2. Revoked-and-old GC. Only fresh-revoked-old (40 d old) matches;
	//    expired-revoked-recent and fresh-revoked-recent are inside the
	//    30 d grace window and survive.
	cutoff := pgTimestamptz(now.Add(-30 * 24 * time.Hour))
	n, err = q.DeleteRevokedRefreshTokensOlderThan(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "only fresh-revoked-old (40 d) is older than the 30 d cutoff")

	after2 := countLabels(t)
	require.False(t, after2["fresh-revoked-old"], "fresh-revoked-old dropped by grace GC")
	require.True(t, after2["expired-revoked-recent"], "expired-revoked-recent inside grace window survives")
	require.True(t, after2["fresh-revoked-recent"], "fresh-revoked-recent inside grace window survives")
	require.True(t, after2["fresh-active"], "fresh-active never matches either query")
}

// ptrTime is a tiny helper to take the address of a time.Time literal.
func ptrTime(t time.Time) *time.Time { return &t }
