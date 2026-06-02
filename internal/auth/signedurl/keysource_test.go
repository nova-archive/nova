package signedurl_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/stretchr/testify/require"
)

type fakeSKQ struct {
	rows   map[string]gen.GetSigningKeyByKIDRow
	active *gen.GetActiveSigningKeyRow
	calls  int
}

func (f *fakeSKQ) GetSigningKeyByKID(_ context.Context, kid string) (gen.GetSigningKeyByKIDRow, error) {
	f.calls++
	r, ok := f.rows[kid]
	if !ok {
		return gen.GetSigningKeyByKIDRow{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeSKQ) GetActiveSigningKey(_ context.Context) (gen.GetActiveSigningKeyRow, error) {
	if f.active == nil {
		return gen.GetActiveSigningKeyRow{}, pgx.ErrNoRows
	}
	return *f.active, nil
}

// identityUnwrap treats the wrapped bytes as the secret; the real keystore
// unwrap is covered by envelope's tests and the M7 integration test.
type identityUnwrap struct{}

func (identityUnwrap) Unwrap(_ context.Context, wrapped []byte, _ uuid.UUID) ([]byte, error) {
	return wrapped, nil
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func skByKID(kid string, state gen.KeyState, secret []byte, retire pgtype.Timestamptz) gen.GetSigningKeyByKIDRow {
	return gen.GetSigningKeyByKIDRow{
		Kid:                kid,
		WrappedKey:         secret,
		MasterKeyVersionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		State:              state,
		RetireAfter:        retire,
	}
}

func TestKeySourceByKID(t *testing.T) {
	t.Parallel()
	now := time.Now()
	secret := []byte("0123456789abcdef0123456789abcdef")
	q := &fakeSKQ{rows: map[string]gen.GetSigningKeyByKIDRow{
		"active":   skByKID("active", gen.KeyStateActive, secret, pgtype.Timestamptz{}),
		"grace":    skByKID("grace", gen.KeyStateRetired, secret, ts(now.Add(time.Hour))),
		"expired":  skByKID("expired", gen.KeyStateRetired, secret, ts(now.Add(-time.Hour))),
		"shredded": skByKID("shredded", gen.KeyStateShredded, secret, pgtype.Timestamptz{}),
	}}
	ks := signedurl.NewKeySource(q, identityUnwrap{}, 0) // no cache

	k, err := ks.ByKID(context.Background(), "active")
	require.NoError(t, err)
	require.Equal(t, secret, k.Secret)
	require.Equal(t, "active", k.KID)

	_, err = ks.ByKID(context.Background(), "grace")
	require.NoError(t, err, "within-grace retired key verifies")

	_, err = ks.ByKID(context.Background(), "expired")
	require.ErrorIs(t, err, signedurl.ErrUnknownKID, "past-grace retired key rejected")

	_, err = ks.ByKID(context.Background(), "shredded")
	require.ErrorIs(t, err, signedurl.ErrUnknownKID)

	_, err = ks.ByKID(context.Background(), "nope")
	require.ErrorIs(t, err, signedurl.ErrUnknownKID)
}

func TestKeySourceActive(t *testing.T) {
	t.Parallel()
	secret := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	active := gen.GetActiveSigningKeyRow{
		Kid:                "k_active",
		WrappedKey:         secret,
		MasterKeyVersionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		State:              gen.KeyStateActive,
	}
	ks := signedurl.NewKeySource(&fakeSKQ{active: &active}, identityUnwrap{}, 0)
	k, err := ks.Active(context.Background())
	require.NoError(t, err)
	require.Equal(t, "k_active", k.KID)
	require.Equal(t, secret, k.Secret)

	_, err = signedurl.NewKeySource(&fakeSKQ{}, identityUnwrap{}, 0).Active(context.Background())
	require.ErrorIs(t, err, signedurl.ErrUnknownKID)
}

func TestKeySourceCacheAndInvalidate(t *testing.T) {
	t.Parallel()
	secret := []byte("0123456789abcdef0123456789abcdef")
	q := &fakeSKQ{rows: map[string]gen.GetSigningKeyByKIDRow{
		"k": skByKID("k", gen.KeyStateActive, secret, pgtype.Timestamptz{}),
	}}
	ks := signedurl.NewKeySource(q, identityUnwrap{}, time.Hour)

	_, err := ks.ByKID(context.Background(), "k")
	require.NoError(t, err)
	require.Equal(t, 1, q.calls)

	_, err = ks.ByKID(context.Background(), "k") // cache hit, no new DB call
	require.NoError(t, err)
	require.Equal(t, 1, q.calls)

	// Underlying row shredded, but the cache still serves the old key until Invalidate.
	q.rows["k"] = skByKID("k", gen.KeyStateShredded, secret, pgtype.Timestamptz{})
	_, err = ks.ByKID(context.Background(), "k")
	require.NoError(t, err)
	require.Equal(t, 1, q.calls)

	ks.Invalidate()
	_, err = ks.ByKID(context.Background(), "k")
	require.ErrorIs(t, err, signedurl.ErrUnknownKID)
	require.Equal(t, 2, q.calls)
}
