package masterkey

// Internal tests for unexported drain methods (Tasks 3-4).
// Helpers that need the DB (seedDEKUnderV1, seedSigningKeyUnderV1) are defined
// here alongside the rotator constructors. Because both test files must share a
// single *Rotator we replicate the setup inline rather than cross-package-sharing
// helpers.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
)

// newInternalTestRotator is a copy of newTestRotator for internal-package tests.
func newInternalTestRotator(t *testing.T) (*Rotator, *pgxpool.Pool, *envelope.Keystore) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	pool := dbtest.New(t, ctx)

	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKeyInternal())
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKeyInternal())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	if err != nil {
		t.Fatalf("NewKeystoreFromEnv: %v", err)
	}
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO master_key_versions (version_label, state) VALUES ('v1', 'active') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert v1 row: %v", err)
	}
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	q := gen.New(pool)
	r := NewRotator(Config{Q: q, Pool: pool, Keystore: ks})
	return r, pool, ks
}

func mustHexKeyInternal() string {
	b := make([]byte, envelope.KeySize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// seedDEKUnderV1 inserts a DEK row encrypted under the non-active v1 version
// by using the low-level WrapKey directly with the raw v1 key bytes.
func seedDEKUnderV1(t *testing.T, pool *pgxpool.Pool, legalHold bool) (id pgtype.UUID, pbk []byte) {
	t.Helper()
	ctx := context.Background()
	mk, err := hex.DecodeString(os.Getenv("NOVA_MASTER_KEY_V1"))
	if err != nil {
		t.Fatal(err)
	}
	pbk = make([]byte, 32)
	if _, err := rand.Read(pbk); err != nil {
		t.Fatal(err)
	}
	wrapped, err := envelope.WrapKey(mk, pbk) // 72-byte wrap under v1
	if err != nil {
		t.Fatal(err)
	}
	var v1id pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM master_key_versions WHERE version_label='v1'`).Scan(&v1id); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state, legal_hold)
		 VALUES ('XChaCha20-Poly1305',$1,$2,'active',$3) RETURNING id`,
		wrapped, v1id, legalHold).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, pbk
}

func TestDrainDEKs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r, pool, ks := newInternalTestRotator(t)
	ctx := context.Background()
	id1, pbk1 := seedDEKUnderV1(t, pool, false)
	seedDEKUnderV1(t, pool, true) // legal_hold must STILL be re-wrapped
	// a shredded DEK that must be skipped:
	var v1id pgtype.UUID
	pool.QueryRow(ctx, `SELECT id FROM master_key_versions WHERE version_label='v1'`).Scan(&v1id)
	var shredID pgtype.UUID
	pool.QueryRow(ctx, `INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
		VALUES ('XChaCha20-Poly1305',$1,$2,'shredded') RETURNING id`, make([]byte, 72), v1id).Scan(&shredID)

	if err := r.drainDEKs(ctx, "v1"); err != nil {
		t.Fatalf("drainDEKs: %v", err)
	}

	// 0 active/rotating v1 DEKs remain
	if n, _ := gen.New(pool).CountDEKsForVersion(ctx, v1id); n != 0 {
		t.Fatalf("remaining v1 DEKs = %d, want 0", n)
	}
	// id1 now references v2 and unwraps to the original plaintext
	var w []byte
	var mv pgtype.UUID
	pool.QueryRow(ctx, `SELECT wrapped_key, master_key_version_id FROM data_encryption_keys WHERE id=$1`, id1).Scan(&w, &mv)
	got, err := ks.Unwrap(ctx, w, uuid.UUID(mv.Bytes))
	if err != nil || !bytes.Equal(got, pbk1) {
		t.Fatalf("re-wrapped DEK mismatch (err %v)", err)
	}
	// shredded row untouched (still v1, still shredded)
	var st string
	var smv pgtype.UUID
	pool.QueryRow(ctx, `SELECT state, master_key_version_id FROM data_encryption_keys WHERE id=$1`, shredID).Scan(&st, &smv)
	if st != "shredded" || smv != v1id {
		t.Fatal("shredded DEK must not be re-wrapped")
	}
}

func TestDrainDEKsZeroesTransientKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	r, pool, _ := newInternalTestRotator(t)
	seedDEKUnderV1(t, pool, false)
	var captured []byte
	r.SetCaptureKeyForTest(func(b []byte) { captured = b }) // slice header saved; backing array shared
	if err := r.drainDEKs(context.Background(), "v1"); err != nil {
		t.Fatal(err)
	}
	if len(captured) == 0 {
		t.Fatal("captureKey never invoked")
	}
	for _, x := range captured {
		if x != 0 {
			t.Fatal("transient plaintext key not zeroed after re-wrap")
		}
	}
}

// --- Task 4 helpers ----------------------------------------------------------

// signingKeyResult carries the kid + original 32-byte secret so tests can
// verify re-wrapping preserved the plaintext.
type signingKeyResult struct {
	kid    string
	secret []byte
}

// seedSigningKeyUnderV1 inserts a signing_keys row encrypted directly under the
// raw v1 master key (bypassing the keystore's active-key gate). Returns the
// original 32-byte secret so the test can re-derive and compare after drain.
func seedSigningKeyUnderV1(t *testing.T, pool *pgxpool.Pool, state string, retireAfter *time.Time) signingKeyResult {
	t.Helper()
	ctx := context.Background()
	mk, err := hex.DecodeString(os.Getenv("NOVA_MASTER_KEY_V1"))
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	wrapped, err := envelope.WrapKey(mk, secret)
	if err != nil {
		t.Fatal(err)
	}
	var v1id pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM master_key_versions WHERE version_label='v1'`).Scan(&v1id); err != nil {
		t.Fatalf("seedSigningKeyUnderV1: scan v1 id: %v", err)
	}
	var ra pgtype.Timestamptz
	if retireAfter != nil {
		ra = pgtype.Timestamptz{Time: *retireAfter, Valid: true}
	}
	kid := "kid-" + hex.EncodeToString(secret[:4])
	_, err = pool.Exec(ctx,
		`INSERT INTO signing_keys (kid, algorithm, wrapped_key, master_key_version_id, state, active_from, retire_after)
		 VALUES ($1,'HMAC-SHA256',$2,$3,$4, now(), $5)`,
		kid, wrapped, v1id, state, ra)
	if err != nil {
		t.Fatalf("seedSigningKeyUnderV1: insert: %v", err)
	}
	return signingKeyResult{kid: kid, secret: secret}
}

// markVersionRotating sets a master_key_versions row to state='rotating'.
func markVersionRotating(t *testing.T, pool *pgxpool.Pool, label string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE master_key_versions SET state='rotating' WHERE version_label=$1`, label); err != nil {
		t.Fatalf("markVersionRotating: %v", err)
	}
}

// newInternalTestRotatorWith is like newInternalTestRotator but accepts an
// OnSigningRewrap callback for Task 4 drain tests.
func newInternalTestRotatorWith(t *testing.T, onSign func()) (*Rotator, *pgxpool.Pool, *envelope.Keystore) {
	t.Helper()
	r, pool, ks := newInternalTestRotator(t)
	r.onSign = onSign
	return r, pool, ks
}

// seedKeystoreV2Only creates a keystore with ONLY v2 loaded (active=v2) plus a
// v1 master_key_versions row inserted directly (not via Bootstrap, since v1
// key is not in env). Used for the stall test.
//
// It temporarily unsets NOVA_MASTER_KEY_V1 (if present) so the keystore scanner
// does not treat "v1" as a declared label. t.Setenv cannot unset a variable, so
// we use os.Unsetenv directly and restore on cleanup.
func seedKeystoreV2Only(t *testing.T, pool *pgxpool.Pool) *envelope.Keystore {
	t.Helper()
	ctx := context.Background()

	// Unset V1 for the duration of this test so the keystore does not try to load it.
	prevV1, hadV1 := os.LookupEnv("NOVA_MASTER_KEY_V1")
	os.Unsetenv("NOVA_MASTER_KEY_V1")
	t.Cleanup(func() {
		if hadV1 {
			os.Setenv("NOVA_MASTER_KEY_V1", prevV1)
		}
	})

	// Set only v2 key + active label.
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKeyInternal())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	if err != nil {
		t.Fatalf("seedKeystoreV2Only: NewKeystoreFromEnv: %v", err)
	}
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("seedKeystoreV2Only: Bootstrap: %v", err)
	}
	// Insert a v1 row manually (simulating a leftover row from a prior rotation).
	_, err = pool.Exec(ctx,
		`INSERT INTO master_key_versions (version_label, state) VALUES ('v1', 'active') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("seedKeystoreV2Only: insert v1 row: %v", err)
	}
	// Re-Bootstrap so the keystore caches the v1 UUID (needed for queries to work).
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("seedKeystoreV2Only: second Bootstrap: %v", err)
	}
	return ks
}

// unwrapSigningUnderKeystore looks up a signing key row by kid, reads its
// current wrapped_key + master_key_version_id, and unwraps via the keystore.
func unwrapSigningUnderKeystore(t *testing.T, pool *pgxpool.Pool, ks *envelope.Keystore, kid string) []byte {
	t.Helper()
	ctx := context.Background()
	var wrappedKey []byte
	var mv pgtype.UUID
	if err := pool.QueryRow(ctx,
		`SELECT wrapped_key, master_key_version_id FROM signing_keys WHERE kid=$1`, kid,
	).Scan(&wrappedKey, &mv); err != nil {
		t.Fatalf("unwrapSigningUnderKeystore: %v", err)
	}
	got, err := ks.Unwrap(ctx, wrappedKey, uuid.UUID(mv.Bytes))
	if err != nil {
		t.Fatalf("unwrapSigningUnderKeystore: Unwrap: %v", err)
	}
	return got
}

// --- Task 4 tests ------------------------------------------------------------

// TestFullDrainRetiresAndRewrapsSigning verifies the complete rotation drain:
// DEKs migrated, active + within-grace-retired signing keys re-wrapped, version
// retired, and the OnSigningRewrap callback invoked.
func TestFullDrainRetiresAndRewrapsSigning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	var invalidated bool
	r, pool, ks := newInternalTestRotatorWith(t, func() { invalidated = true })
	ctx := context.Background()

	// Seed: one DEK, one active signing key, one within-grace retired signing key.
	seedDEKUnderV1(t, pool, false)
	skActive := seedSigningKeyUnderV1(t, pool, "active", nil)
	retireAfter := time.Now().Add(time.Hour)
	skRetired := seedSigningKeyUnderV1(t, pool, "retired", &retireAfter)

	// Start moves v1 to 'rotating' and enqueues the drain job.
	if err := r.Start(ctx, "v1", "v2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// drainOnceForTest drives the full drain synchronously.
	r.drainOnceForTest(ctx, "v1", "v2")

	var v1id pgtype.UUID
	pool.QueryRow(ctx, `SELECT id FROM master_key_versions WHERE version_label='v1'`).Scan(&v1id)

	// All DEKs on v1 must be gone.
	if n, _ := gen.New(pool).CountDEKsForVersion(ctx, v1id); n != 0 {
		t.Fatalf("remaining v1 DEKs = %d, want 0", n)
	}
	// All signing keys on v1 must be gone.
	if n, _ := gen.New(pool).CountSigningKeysForVersion(ctx, v1id); n != 0 {
		t.Fatalf("remaining v1 signing keys = %d, want 0", n)
	}
	// v1 must be retired.
	row, err := gen.New(pool).GetMasterVersionByLabel(ctx, "v1")
	if err != nil {
		t.Fatalf("GetMasterVersionByLabel: %v", err)
	}
	if row.State != gen.KeyStateRetired {
		t.Fatalf("v1 state = %v, want retired", row.State)
	}
	// Active signing key must unwrap to original secret under v2.
	if got := unwrapSigningUnderKeystore(t, pool, ks, skActive.kid); !bytes.Equal(got, skActive.secret) {
		t.Fatal("active signing key: re-wrapped value does not match original secret")
	}
	// Within-grace retired signing key must also unwrap correctly.
	if got := unwrapSigningUnderKeystore(t, pool, ks, skRetired.kid); !bytes.Equal(got, skRetired.secret) {
		t.Fatal("retired signing key: re-wrapped value does not match original secret")
	}
	// Callback must have fired.
	if !invalidated {
		t.Fatal("OnSigningRewrap was not invoked")
	}
}

// --- Task 5 tests ------------------------------------------------------------

// TestStatusAndReadyz covers the Status projection and Readyz stall-detection:
//   - idle (no rotation in progress): InProgress == nil, Readyz == nil
//   - rotating with key loaded: InProgress populated, Stalled=false, Readyz == nil
//   - rotating with key NOT loaded: Stalled=true, Readyz returns an error
func TestStatusAndReadyz(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	r, pool, _ := newInternalTestRotator(t)
	ctx := context.Background()

	// --- idle: no rotation in progress ---
	st, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("idle Status: %v", err)
	}
	if st.InProgress != nil {
		t.Fatalf("idle: InProgress must be nil, got %+v", st.InProgress)
	}
	if err := r.Readyz(ctx); err != nil {
		t.Fatalf("idle Readyz: %v", err)
	}

	// --- rotating with key loaded ---
	seedDEKUnderV1(t, pool, false)
	markVersionRotating(t, pool, "v1")

	st, err = r.Status(ctx)
	if err != nil {
		t.Fatalf("rotating(loaded) Status: %v", err)
	}
	if st.InProgress == nil {
		t.Fatal("rotating(loaded): InProgress must not be nil")
	}
	if st.InProgress.From != "v1" {
		t.Fatalf("rotating(loaded): From = %q, want %q", st.InProgress.From, "v1")
	}
	if st.InProgress.Stalled {
		t.Fatalf("rotating(loaded): Stalled must be false, got %+v", st.InProgress)
	}
	if st.InProgress.RemainingDEKs < 1 {
		t.Fatalf("rotating(loaded): RemainingDEKs = %d, want >= 1", st.InProgress.RemainingDEKs)
	}
	if err := r.Readyz(ctx); err != nil {
		t.Fatalf("loaded Readyz must pass: %v", err)
	}

	// --- rotating with key NOT loaded ---
	// Build a v2-only keystore over the same DB pool (v1 row is already marked rotating).
	ks2 := seedKeystoreV2Only(t, pool)
	r2 := NewRotator(Config{Q: gen.New(pool), Pool: pool, Keystore: ks2})

	st2, err := r2.Status(ctx)
	if err != nil {
		t.Fatalf("rotating(unloaded) Status: %v", err)
	}
	if st2.InProgress == nil || !st2.InProgress.Stalled {
		t.Fatalf("rotating(unloaded): Stalled must be true, got %+v", st2.InProgress)
	}
	if err := r2.Readyz(ctx); err == nil {
		t.Fatal("unloaded Readyz must degrade (return error)")
	}
}

// TestResumeStallsWhenKeyMissing verifies that resumeIfRotating gracefully
// stalls (logs + returns) when the source version's key is not loaded, leaving
// the version in 'rotating' state.
func TestResumeStallsWhenKeyMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := dbtest.New(t, context.Background())
	ks := seedKeystoreV2Only(t, pool)
	// Mark v1 rotating to simulate a crash mid-rotation.
	markVersionRotating(t, pool, "v1")

	q := gen.New(pool)
	r := NewRotator(Config{Q: q, Pool: pool, Keystore: ks})
	// Must not panic and must leave v1 in 'rotating'.
	r.resumeIfRotating(context.Background())

	row, err := gen.New(pool).GetMasterVersionByLabel(context.Background(), "v1")
	if err != nil {
		t.Fatalf("GetMasterVersionByLabel: %v", err)
	}
	if row.State != gen.KeyStateRotating {
		t.Fatalf("stalled rotation must stay 'rotating', got %v", row.State)
	}
}
