package masterkey

// Internal tests for unexported drain methods (Task 3).
// Helpers that need the DB (seedDEKUnderV1) are defined here alongside the
// exported newTestRotatorInternal shim that calls newTestRotator from the
// external test package. Because both test files must share a single *Rotator
// we replicate the setup inline rather than cross-package-sharing helpers.

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
