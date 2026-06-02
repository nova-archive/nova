package signedurl

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// secretSize is the signing-key length: 256 bits, matching SIGNED_URL_FORMAT.md.
const secretSize = 32

// insertSigningKeyQuerier inserts a new active signing_keys row. Both
// *gen.Queries and a tx-scoped *gen.Queries satisfy it.
type insertSigningKeyQuerier interface {
	InsertSigningKey(ctx context.Context, arg gen.InsertSigningKeyParams) error
}

// keyWrapper wraps a secret under the active master key, returning the wrapped
// bytes and the master_key_versions id. *envelope.Keystore satisfies it.
type keyWrapper interface {
	Wrap(secret []byte) ([]byte, uuid.UUID, error)
}

// bootstrapQuerier is what EnsureActiveKey needs.
type bootstrapQuerier interface {
	insertSigningKeyQuerier
	CountActiveSigningKeys(ctx context.Context) (int64, error)
}

// EnsureActiveKey mints an initial active signing key when none exists, so a
// freshly-initialised node verifies and mints signed URLs out of the box.
// Idempotent — a no-op once an active key is present (mirrors the master-key
// bootstrap). Called at coordinator startup after the keystore is bootstrapped.
func EnsureActiveKey(ctx context.Context, q bootstrapQuerier, ks keyWrapper) error {
	n, err := q.CountActiveSigningKeys(ctx)
	if err != nil {
		return fmt.Errorf("signedurl: count active signing keys: %w", err)
	}
	if n > 0 {
		return nil
	}
	_, err = MintKey(ctx, q, ks)
	return err
}

// MintKey generates a fresh 256-bit secret, wraps it under the active master
// key, and inserts a new active signing_keys row with a generated kid. Returns
// the new kid. Used by bootstrap and by rotation (inside a transaction).
func MintKey(ctx context.Context, q insertSigningKeyQuerier, ks keyWrapper) (string, error) {
	secret := make([]byte, secretSize)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("signedurl: generate secret: %w", err)
	}
	wrapped, mvid, err := ks.Wrap(secret)
	if err != nil {
		return "", fmt.Errorf("signedurl: wrap secret: %w", err)
	}
	kid, err := genKID()
	if err != nil {
		return "", err
	}
	if err := q.InsertSigningKey(ctx, gen.InsertSigningKeyParams{
		Kid:                kid,
		WrappedKey:         wrapped,
		MasterKeyVersionID: pgtype.UUID{Bytes: mvid, Valid: true},
	}); err != nil {
		return "", fmt.Errorf("signedurl: insert signing key: %w", err)
	}
	return kid, nil
}

var kidEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// genKID returns an opaque, URL-safe, unique signing-key id ("k_" + lowercase
// base32 of 10 random bytes). The kid carries no parseable meaning.
func genKID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("signedurl: generate kid: %w", err)
	}
	return "k_" + strings.ToLower(kidEncoding.EncodeToString(b)), nil
}
