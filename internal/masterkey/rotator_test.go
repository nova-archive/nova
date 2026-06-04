package masterkey_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/masterkey"
)

func mustHexKey() string {
	b := make([]byte, envelope.KeySize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// newTestRotator builds a Rotator against an ephemeral Postgres container with
// two master-key versions (v1 + v2, active = v2). Both version rows are present
// in master_key_versions and both labels are cached in the keystore.
//
// Tasks 3-5 extend this helper with seedDEKUnderV1 etc. alongside it.
func newTestRotator(t *testing.T) (*masterkey.Rotator, *pgxpool.Pool, *envelope.Keystore) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	pool := dbtest.New(t, ctx)

	// Load v1 and v2; active = v2.
	t.Setenv("NOVA_MASTER_KEY_V1", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_V2", mustHexKey())
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v2")

	ks, err := envelope.NewKeystoreFromEnv(pool)
	if err != nil {
		t.Fatalf("NewKeystoreFromEnv: %v", err)
	}

	// Bootstrap inserts the active (v2) row.
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Insert a v1 row (retired state is fine — Start only checks it is not
	// KeyStateRetired when it's the 'from' label, but we use 'active' here so
	// BeginVersionRotation can flip it to 'rotating').
	_, err = pool.Exec(ctx,
		`INSERT INTO master_key_versions (version_label, state) VALUES ('v1', 'active') ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("insert v1 row: %v", err)
	}

	// Re-Bootstrap so loadVersions caches the v1 UUID in the keystore.
	if _, err := ks.Bootstrap(ctx); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	q := gen.New(pool)
	r := masterkey.NewRotator(masterkey.Config{
		Q:        q,
		Pool:     pool,
		Keystore: ks,
	})

	return r, pool, ks
}

func TestStartValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	r, pool, _ := newTestRotator(t) // v1+v2 rows present, active=v2

	ctx := context.Background()

	// to must equal the active label
	if err := r.Start(ctx, "v1", "v1"); !errors.Is(err, masterkey.ErrToNotActive) {
		t.Fatalf("to=v1 (not active) want ErrToNotActive, got %v", err)
	}
	// unknown/unloaded from
	if err := r.Start(ctx, "v9", "v2"); !errors.Is(err, masterkey.ErrInvalidFrom) {
		t.Fatalf("unknown from want ErrInvalidFrom, got %v", err)
	}
	// happy path marks v1 rotating
	if err := r.Start(ctx, "v1", "v2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	row, err := gen.New(pool).GetRotatingVersion(ctx)
	if err != nil || row.VersionLabel != "v1" {
		t.Fatalf("rotating version = %q (err %v), want v1", row.VersionLabel, err)
	}
	// a second concurrent rotation is refused
	if err := r.Start(ctx, "v1", "v2"); !errors.Is(err, masterkey.ErrAlreadyRotating) {
		t.Fatalf("second Start want ErrAlreadyRotating, got %v", err)
	}
}
