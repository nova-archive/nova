package localissuer

import (
	"context"
	"sync"
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
