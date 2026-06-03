package moderation_test

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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/blobfixture"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/internal/moderation"
)

// fakeBackend records the CIDs passed to Unpin so the tombstone tests can
// assert the parent + derivative were unpinned after commit.
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

// seedParentWithDerivative seeds an active encrypted parent blob and one
// encrypted derivative whose parent_cid points at the parent. The derivative's
// owner is rewritten to the parent's owner so a strike lands on one user.
func seedParentWithDerivative(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ks *envelope.Keystore, be ipfs.Backend) seeded {
	t.Helper()
	parent, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("parent original bytes"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)
	deriv, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("derivative webp bytes"), MIME: "image/webp", Visibility: "private"})
	require.NoError(t, err)

	// Make the second blob a derivative of the first, sharing the parent's owner.
	// derivative_preset/derivative_format are required when parent_cid is set
	// (blobs CHECK derivative_columns_consistent).
	_, err = pool.Exec(ctx,
		`UPDATE blobs SET parent_cid=$1, owner_id=$2, derivative_preset='thumb', derivative_format='webp'
		 WHERE cid=$3`, parent.CID, parent.OwnerID, deriv.CID)
	require.NoError(t, err)

	return seeded{parentCID: parent.CID, derivCID: deriv.CID, ownerID: parent.OwnerID}
}

func newService(pool *pgxpool.Pool, be moderation.Backend, clock func() time.Time) *moderation.Service {
	return moderation.NewService(
		gen.New(pool), pool, be, stateCascade,
		auditlog.NewWriter(gen.New(pool), slog.Default()), slog.Default(), clock)
}

// newOperator inserts an operator user and returns its id, suitable as a
// moderation_decisions.decided_by / audit_log.actor_id (both FK users.id).
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

func countRevocations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM signed_url_revocations WHERE kind='cid' AND value=$1`, cidStr).Scan(&n))
	return n
}

func strikesFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, owner uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT coalesce((SELECT strikes FROM takedown_repeat_infringers WHERE user_id=$1),0)`, owner).Scan(&n))
	return n
}

func countAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action, cidStr string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action=$1 AND target_type='cid' AND target_id=$2`,
		action, cidStr).Scan(&n))
	return n
}

func schedTombstoneSet(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cidStr, action string) bool {
	t.Helper()
	var set bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT scheduled_tombstone_at IS NOT NULL FROM moderation_decisions
		 WHERE cid=$1 AND action=$2 ORDER BY decided_at DESC LIMIT 1`, cidStr, action).Scan(&set))
	return set
}

// --- Task 2: Quarantine ------------------------------------------------------

func TestQuarantineCascadesAndAudits(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "dmca", Reason: "DMCA notice",
		TombstoneAfter: 14 * 24 * time.Hour, Actor: &opID,
	}))

	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.parentCID))
	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.derivCID), "derivative cascades to quarantined")

	require.True(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"scheduled_tombstone_at must be set when not legal_hold")
	var legalHold bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT legal_hold FROM moderation_decisions WHERE cid=$1 AND action='quarantine'`,
		s.parentCID).Scan(&legalHold))
	require.False(t, legalHold)

	require.Equal(t, 1, countRevocations(t, ctx, pool, s.parentCID), "a ('cid', cid) revocation exists")
	require.Equal(t, 1, strikesFor(t, ctx, pool, s.ownerID), "owner has one strike")
	require.Equal(t, 1, countAudit(t, ctx, pool, "dmca.quarantined", s.parentCID))
}

// An anonymous / public_archival upload has owner_id NULL. Quarantine and
// tombstone must not crash on the NULL owner projection (GetBlobForModeration)
// and must skip the repeat-infringer strike. Actor is nil here too, exercising
// the system-action path the scheduled-tombstone sweep uses.
func TestQuarantineAndTombstoneOwnerlessBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	ks := newKeystore(t, ctx, pool)
	be := newEmbeddedBackend(t, ctx)

	b, err := blobfixture.Seed(ctx, blobfixture.Deps{Pool: pool, Backend: be, Keystore: ks},
		blobfixture.Spec{Plaintext: []byte("ownerless bytes"), MIME: "image/png", Visibility: "private"})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE blobs SET owner_id=NULL WHERE cid=$1`, b.CID)
	require.NoError(t, err)

	svc := newService(pool, &fakeBackend{}, time.Now)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: b.CID, Rule: "dmca", Reason: "ownerless", TombstoneAfter: time.Hour,
	}))
	require.Equal(t, "quarantined", blobState(t, ctx, pool, b.CID))

	var strikeRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM takedown_repeat_infringers`).Scan(&strikeRows))
	require.Equal(t, 0, strikeRows, "no strike when the blob has no owner")

	require.NoError(t, svc.Tombstone(ctx, moderation.TombstoneCmd{CID: b.CID, Rule: "dmca", Reason: "ownerless"}))
	require.Equal(t, "tombstoned", blobState(t, ctx, pool, b.CID))
}

func TestQuarantineLegalHoldSetsDEKAndNoSchedule(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "severe_content", Reason: "severe", LegalHold: true, Actor: &opID,
	}))

	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.parentCID))

	pState, pHold, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "active", pState, "DEK is not shredded by quarantine")
	require.True(t, pHold, "parent DEK legal_hold=true")
	dState, dHold, _ := dekFor(t, ctx, pool, s.derivCID)
	require.Equal(t, "active", dState)
	require.True(t, dHold, "derivative DEK legal_hold=true")

	require.False(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"scheduled_tombstone_at IS NULL under legal hold")
	require.Equal(t, 1, countAudit(t, ctx, pool, "severe.quarantined", s.parentCID))
}

// --- Task 3: Tombstone -------------------------------------------------------

func TestTombstoneShredsCascadesAndUnpins(t *testing.T) {
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
	fb := &fakeBackend{}
	svc := newService(pool, fb, time.Now)

	// Quarantine first (sets the originating scheduled_tombstone_at), then tombstone.
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "dmca", Reason: "notice", TombstoneAfter: 14 * 24 * time.Hour, Actor: &opID,
	}))
	require.True(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"))

	require.NoError(t, svc.Tombstone(ctx, moderation.TombstoneCmd{
		CID: s.parentCID, Rule: "dmca", Reason: "takedown", Actor: &opID,
	}))

	require.Equal(t, "tombstoned", blobState(t, ctx, pool, s.parentCID))
	require.Equal(t, "tombstoned", blobState(t, ctx, pool, s.derivCID), "derivative cascades to tombstoned")

	pState, _, pWrapped := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "shredded", pState, "parent DEK shredded")
	require.Equal(t, zeros72Test(), pWrapped, "parent wrapped_key zeroed")
	dState, _, dWrapped := dekFor(t, ctx, pool, s.derivCID)
	require.Equal(t, "shredded", dState, "derivative DEK shredded")
	require.Equal(t, zeros72Test(), dWrapped, "derivative wrapped_key zeroed")

	require.False(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"originating quarantine scheduled_tombstone_at cleared")
	require.Equal(t, 1, countRevocations(t, ctx, pool, s.parentCID))
	require.Equal(t, 1, countAudit(t, ctx, pool, "dmca.tombstoned", s.parentCID))

	require.True(t, fb.saw(s.parentCID), "backend unpinned the parent")
	require.True(t, fb.saw(s.derivCID), "backend unpinned the derivative")
}

func TestTombstoneRefusedUnderLegalHold(t *testing.T) {
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
	fb := &fakeBackend{}
	svc := newService(pool, fb, time.Now)

	// Quarantine under legal hold: DEK.legal_hold=true, no schedule.
	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "severe_content", Reason: "severe", LegalHold: true, Actor: &opID,
	}))

	err := svc.Tombstone(ctx, moderation.TombstoneCmd{CID: s.parentCID, Rule: "severe_content", Reason: "x", Actor: &opID})
	require.ErrorIs(t, err, moderation.ErrLegalHold, "tombstone must be refused by the no_shred_under_legal_hold CHECK")

	// The whole tx rolled back: nothing tombstoned, nothing shredded.
	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.parentCID), "state unchanged")
	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.derivCID))
	pState, pHold, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "active", pState, "parent DEK still active (un-shredded)")
	require.True(t, pHold, "legal_hold still set")
	require.Equal(t, 0, countAudit(t, ctx, pool, "dmca.tombstoned", s.parentCID), "no tombstone audit row")
	require.False(t, fb.saw(s.parentCID), "no unpin on a refused tombstone")
}

func zeros72Test() []byte { return make([]byte, 72) }

// --- Task 4: ClearLegalHold / Restore / CounterNotice ------------------------

func TestClearLegalHoldThenTombstoneSucceeds(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)

	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "severe_content", Reason: "severe", LegalHold: true, Actor: &opID,
	}))
	// Tombstone is refused while held.
	require.ErrorIs(t, svc.Tombstone(ctx, moderation.TombstoneCmd{CID: s.parentCID, Reason: "x", Actor: &opID}),
		moderation.ErrLegalHold)

	require.NoError(t, svc.ClearLegalHold(ctx, s.parentCID, "", "released after review", &opID))

	// DEK legal_hold cleared on the tree, decision schedule set to now, audit row.
	_, pHold, _ := dekFor(t, ctx, pool, s.parentCID)
	require.False(t, pHold, "parent DEK legal_hold cleared")
	_, dHold, _ := dekFor(t, ctx, pool, s.derivCID)
	require.False(t, dHold, "derivative DEK legal_hold cleared")
	require.True(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"clear-legal-hold sets scheduled_tombstone_at=now()")
	var stillHeld bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT legal_hold FROM moderation_decisions WHERE cid=$1 AND action='quarantine'`,
		s.parentCID).Scan(&stillHeld))
	require.False(t, stillHeld, "decision legal_hold cleared")
	require.Equal(t, 1, countAudit(t, ctx, pool, "severe.legal_hold_cleared", s.parentCID))

	// Now the same tombstone shreds (exit criterion #3).
	require.NoError(t, svc.Tombstone(ctx, moderation.TombstoneCmd{CID: s.parentCID, Reason: "tombstone after clear", Actor: &opID}))
	require.Equal(t, "tombstoned", blobState(t, ctx, pool, s.parentCID))
	pState, _, _ := dekFor(t, ctx, pool, s.parentCID)
	require.Equal(t, "shredded", pState)
}

func TestRestoreReactivatesAndKeepsRevocation(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)

	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "dmca", Reason: "notice", TombstoneAfter: 14 * 24 * time.Hour, Actor: &opID,
	}))
	require.Equal(t, 1, countRevocations(t, ctx, pool, s.parentCID))

	require.NoError(t, svc.Restore(ctx, s.parentCID, "counter-notice upheld", &opID))

	require.Equal(t, "active", blobState(t, ctx, pool, s.parentCID))
	require.Equal(t, "active", blobState(t, ctx, pool, s.derivCID), "derivative cascades back to active")
	require.False(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"restore clears scheduled_tombstone_at")
	require.Equal(t, 1, countRevocations(t, ctx, pool, s.parentCID),
		"restore must NOT delete the ('cid', cid) revocation (no issuer column)")
	require.Equal(t, 1, countAudit(t, ctx, pool, "dmca.restored", s.parentCID))
}

func TestRestoreNonQuarantinedFails(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)

	// Blob is still active (never quarantined) → restore is a conflict.
	require.ErrorIs(t, svc.Restore(ctx, s.parentCID, "noop", &opID), moderation.ErrNotQuarantined)
	require.Equal(t, "active", blobState(t, ctx, pool, s.parentCID))
}

func TestCounterNoticeClearsScheduleKeepsQuarantined(t *testing.T) {
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
	svc := newService(pool, &fakeBackend{}, time.Now)

	require.NoError(t, svc.Quarantine(ctx, moderation.QuarantineCmd{
		CID: s.parentCID, Rule: "dmca", Reason: "notice", TombstoneAfter: 14 * 24 * time.Hour, Actor: &opID,
	}))
	require.True(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"))

	require.NoError(t, svc.CounterNotice(ctx, s.parentCID, "user disputes the claim", &opID))

	require.False(t, schedTombstoneSet(t, ctx, pool, s.parentCID, "quarantine"),
		"counter-notice clears scheduled_tombstone_at")
	require.Equal(t, "quarantined", blobState(t, ctx, pool, s.parentCID), "blob stays quarantined")
	require.Equal(t, 1, countAudit(t, ctx, pool, "dmca.counter_received", s.parentCID))
}
