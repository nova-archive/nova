package lifecycle_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/lifecycle"
)

// fakeBackend records the CIDs passed to Unpin so the sweep tests can assert the
// parent + derivative were unpinned after commit.
type fakeBackend struct {
	mu       sync.Mutex
	unpinned []string
}

func (f *fakeBackend) Unpin(_ context.Context, c cid.Cid) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpinned = append(f.unpinned, c.String())
	return nil
}

func (f *fakeBackend) saw(cidStr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.unpinned {
		if u == cidStr {
			return true
		}
	}
	return false
}

// clock is a mutable test clock. SoftDelete stamps soft_deleted_at via SQL now();
// the sweep's cutoff uses the Service clock, so advancing this past the grace
// window makes a soft-delete overdue without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stateCascade mirrors nova-image's OnDelete: it propagates the parent's new
// state to its derivatives so the tests can assert the derivative transitions.
func stateCascade(ctx context.Context, tx pgx.Tx, parentCID, newState string) error {
	_, err := tx.Exec(ctx, "UPDATE blobs SET state=$1 WHERE parent_cid=$2", newState, parentCID)
	return err
}

func newEmbeddedBackend(t *testing.T, ctx context.Context) ipfs.Backend {
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

func newKeystore(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *envelope.Keystore {
	t.Helper()
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	return ks
}

// seeded is a parent blob with one derivative, sharing a single owner.
type seeded struct {
	parentCID string
	derivCID  string
	ownerID   uuid.UUID
}

func seedParentWithDerivative(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ks *envelope.Keystore, be ipfs.Backend) seeded {
	t.Helper()
	parent, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("parent original bytes"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)
	deriv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("derivative webp bytes"), MIME: "image/webp", Visibility: "private"})
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`UPDATE blobs SET parent_cid=$1, owner_id=$2, derivative_preset='thumb', derivative_format='webp'
		 WHERE cid=$3`, parent.CID, parent.OwnerID, deriv.CID)
	require.NoError(t, err)
	return seeded{parentCID: parent.CID, derivCID: deriv.CID, ownerID: parent.OwnerID}
}

func newService(pool *pgxpool.Pool, be lifecycle.Backend, now func() time.Time, grace time.Duration) *lifecycle.Service {
	return lifecycle.NewService(
		gen.New(pool), pool, be, stateCascade,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), now, grace)
}

func newOperator(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		"op-"+uuid.NewString()+"@fixture.test").Scan(&id))
	return id
}

// --- assertion helpers -------------------------------------------------------

func blobState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state FROM blobs WHERE cid=$1`, cidStr).Scan(&s))
	return s
}

func softDeletedAtSet(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) bool {
	t.Helper()
	var ts pgtype.Timestamptz
	require.NoError(t, pool.QueryRow(ctx, `SELECT soft_deleted_at FROM blobs WHERE cid=$1`, cidStr).Scan(&ts))
	return ts.Valid
}

// dekFor returns (state, legal_hold, wrapped_key) for the blob's DEK.
func dekFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) (string, bool, []byte) {
	t.Helper()
	var (
		state   string
		hold    bool
		wrapped []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT k.state, k.legal_hold, k.wrapped_key
		 FROM data_encryption_keys k JOIN blobs b ON b.encryption_key_id = k.id
		 WHERE b.cid=$1`, cidStr).Scan(&state, &hold, &wrapped))
	return state, hold, wrapped
}

func setDEKLegalHold(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`UPDATE data_encryption_keys k SET legal_hold=true
		 FROM blobs b WHERE b.encryption_key_id=k.id AND b.cid=$1`, cidStr)
	require.NoError(t, err)
}

func countAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action, cidStr string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action=$1 AND target_type='cid' AND target_id=$2`,
		action, cidStr).Scan(&n))
	return n
}

func zeros72() []byte { return make([]byte, 72) }

// --- SoftDelete --------------------------------------------------------------

func TestSoftDeleteFlipsStateCascadesAndAudits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)
	s := seedParentWithDerivative(t, ctx, pool, ks, be)

	opID := newOperator(t, ctx, pool)
	svc := newService(pool, &fakeBackend{}, time.Now, 24*time.Hour)

	require.NoError(t, svc.SoftDelete(ctx, s.parentCID, &opID))

	require.Equal(t, "soft_deleted", blobState(t, ctx, pool, s.parentCID))
	require.Equal(t, "soft_deleted", blobState(t, ctx, pool, s.derivCID), "derivative cascades to soft_deleted")
	require.True(t, softDeletedAtSet(t, ctx, pool, s.parentCID), "soft_deleted_at stamped")
	require.Equal(t, 1, countAudit(t, ctx, pool, "blob.soft_deleted", s.parentCID))

	// DEK is untouched while soft-deleted (reversible during the grace window).
	pState, _, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "active", pState, "DEK not shredded by soft-delete")
}

func TestSoftDeleteNonActiveReturnsErrNotActive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)
	s := seedParentWithDerivative(t, ctx, pool, ks, be)

	svc := newService(pool, &fakeBackend{}, time.Now, 24*time.Hour)
	require.NoError(t, svc.SoftDelete(ctx, s.parentCID, nil))
	// Second delete on an already-soft-deleted blob is a conflict.
	require.ErrorIs(t, svc.SoftDelete(ctx, s.parentCID, nil), lifecycle.ErrNotActive)
	// An unknown CID is also not active.
	require.ErrorIs(t, svc.SoftDelete(ctx, "bafyunknowncid", nil), lifecycle.ErrNotActive)
}

// --- Sweep -------------------------------------------------------------------

func TestSweepTombstonesOverdueShredsAndUnpins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)
	s := seedParentWithDerivative(t, ctx, pool, ks, be)

	fb := &fakeBackend{}
	clk := &clock{t: time.Now()}
	svc := newService(pool, fb, clk.now, 24*time.Hour)
	sweeper := lifecycle.NewSweeper(svc, time.Minute, true, slog.Default())

	require.NoError(t, svc.SoftDelete(ctx, s.parentCID, nil))

	// Not yet overdue (cutoff = now-24h): the sweep leaves it alone.
	sweeper.Tick(ctx)
	require.Equal(t, "soft_deleted", blobState(t, ctx, pool, s.parentCID), "not overdue yet")
	pState, _, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "active", pState)

	// Advance past the grace window → overdue → tombstone + crypto-shred.
	clk.advance(48 * time.Hour)
	sweeper.Tick(ctx)

	require.Equal(t, "tombstoned", blobState(t, ctx, pool, s.parentCID))
	require.Equal(t, "tombstoned", blobState(t, ctx, pool, s.derivCID), "derivative cascades to tombstoned")

	pState, _, pWrapped := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "shredded", pState, "parent DEK shredded")
	require.Equal(t, zeros72(), pWrapped, "parent wrapped_key zeroed")
	dState, _, dWrapped := dekFor(t, ctx, pool, s.derivCID)
	require.Equal(t, "shredded", dState, "derivative DEK shredded")
	require.Equal(t, zeros72(), dWrapped, "derivative wrapped_key zeroed")

	require.Equal(t, 1, countAudit(t, ctx, pool, "blob.tombstoned", s.parentCID),
		"system blob.tombstoned audit row (not a moderation dmca.* action)")
	require.Equal(t, 0, countAudit(t, ctx, pool, "dmca.tombstoned", s.parentCID),
		"owner deletion must not write a moderation audit action")
	require.True(t, fb.saw(s.parentCID), "parent unpinned")
	require.True(t, fb.saw(s.derivCID), "derivative unpinned")
}

func TestSweepSkipsLegalHeldTree(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)
	s := seedParentWithDerivative(t, ctx, pool, ks, be)

	setDEKLegalHold(t, ctx, pool, s.parentCID)

	fb := &fakeBackend{}
	clk := &clock{t: time.Now()}
	svc := newService(pool, fb, clk.now, 24*time.Hour)
	sweeper := lifecycle.NewSweeper(svc, time.Minute, true, slog.Default())

	require.NoError(t, svc.SoftDelete(ctx, s.parentCID, nil))
	clk.advance(48 * time.Hour)
	sweeper.Tick(ctx)

	// Legal-held tree is filtered out of the claim → stays soft_deleted, unshredded.
	require.Equal(t, "soft_deleted", blobState(t, ctx, pool, s.parentCID), "legal hold blocks the sweep")
	pState, pHold, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "active", pState, "DEK not shredded under legal hold")
	require.True(t, pHold)
	require.False(t, fb.saw(s.parentCID), "no unpin on a skipped tree")
}

func TestSweepTombstonesPublicArchivalNoDEK(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	be := newEmbeddedBackend(t, ctx)

	// public_archival blob: encryption_key_id NULL, raw plaintext in Kubo.
	b, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be},
		blobfixture.Spec{Plaintext: []byte("public archival bytes"), MIME: "image/png", Unencrypted: true})
	require.NoError(t, err)

	fb := &fakeBackend{}
	clk := &clock{t: time.Now()}
	svc := newService(pool, fb, clk.now, 24*time.Hour)
	sweeper := lifecycle.NewSweeper(svc, time.Minute, true, slog.Default())

	require.NoError(t, svc.SoftDelete(ctx, b.CID, nil))
	clk.advance(48 * time.Hour)
	sweeper.Tick(ctx)

	require.Equal(t, "tombstoned", blobState(t, ctx, pool, b.CID), "no-DEK blob tombstones cleanly")
	require.True(t, fb.saw(b.CID), "unpinned")
}
