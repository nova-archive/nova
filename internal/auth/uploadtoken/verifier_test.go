package uploadtoken_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/uploadtoken"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/stretchr/testify/require"
)

// fakeQuerier is a minimal in-memory implementation of uploadtoken.Querier.
type fakeQuerier struct {
	tokens      map[uuid.UUID]gen.UploadToken
	touchCalled []uuid.UUID
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{tokens: make(map[uuid.UUID]gen.UploadToken)}
}

func (f *fakeQuerier) GetUploadTokenByID(_ context.Context, id pgtype.UUID) (gen.UploadToken, error) {
	tok, ok := f.tokens[uuid.UUID(id.Bytes)]
	if !ok {
		return gen.UploadToken{}, pgx.ErrNoRows
	}
	return tok, nil
}

func (f *fakeQuerier) TouchUploadTokenUsed(_ context.Context, id pgtype.UUID) error {
	f.touchCalled = append(f.touchCalled, uuid.UUID(id.Bytes))
	return nil
}

// makeToken inserts a valid upload token into the fake querier and returns
// the wire string plus the token row.
func makeToken(t *testing.T, q *fakeQuerier, createdBy uuid.UUID, role gen.UserRole) (wire string, id uuid.UUID) {
	t.Helper()
	var hash string
	var err error
	wire, id, hash, err = uploadtoken.Generate()
	require.NoError(t, err)

	q.tokens[id] = gen.UploadToken{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		TokenHash: hash,
		Role:      role,
		CreatedBy: pgtype.UUID{Bytes: createdBy, Valid: true},
	}
	return wire, id
}

func TestVerify_ValidToken(t *testing.T) {
	q := newFakeQuerier()
	createdBy := uuid.New()
	wire, id := makeToken(t, q, createdBy, gen.UserRoleUploader)

	v := uploadtoken.New(q)
	identity, err := v.Verify(context.Background(), wire)
	require.NoError(t, err)
	require.Equal(t, "uploader", identity.Role)
	require.Equal(t, createdBy.String(), identity.UserID)
	require.Equal(t, id.String(), identity.CredentialID)
	require.Equal(t, "nova:upload-token", identity.Issuer)

	// Touch should have been called
	require.Contains(t, q.touchCalled, id)
}

func TestVerify_RevokedToken(t *testing.T) {
	q := newFakeQuerier()
	createdBy := uuid.New()
	wire, id := makeToken(t, q, createdBy, gen.UserRoleUploader)

	// Mark as revoked
	tok := q.tokens[id]
	tok.RevokedAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	q.tokens[id] = tok

	v := uploadtoken.New(q)
	_, err := v.Verify(context.Background(), wire)
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrTokenNotForMe, "revoked token should not return ErrTokenNotForMe")
}

func TestVerify_ExpiredToken(t *testing.T) {
	q := newFakeQuerier()
	createdBy := uuid.New()
	wire, id := makeToken(t, q, createdBy, gen.UserRoleUploader)

	// Mark as expired
	tok := q.tokens[id]
	tok.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	q.tokens[id] = tok

	v := uploadtoken.New(q)
	_, err := v.Verify(context.Background(), wire)
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrTokenNotForMe, "expired token should not return ErrTokenNotForMe")
}

func TestVerify_WrongSecret(t *testing.T) {
	createdBy := uuid.New()

	// Generate a wire+secret and a separate hash from a different token — the
	// stored hash will not match the wire's secret.
	wire, id, _, err := uploadtoken.Generate()
	require.NoError(t, err)
	_, _, otherHash, err := uploadtoken.Generate()
	require.NoError(t, err)

	q := newFakeQuerier()
	q.tokens[id] = gen.UploadToken{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		TokenHash: otherHash, // intentionally mismatched
		Role:      gen.UserRoleUploader,
		CreatedBy: pgtype.UUID{Bytes: createdBy, Valid: true},
	}

	v := uploadtoken.New(q)
	_, err = v.Verify(context.Background(), wire)
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrTokenNotForMe, "wrong-secret token should not return ErrTokenNotForMe")
}

func TestVerify_NotUploadToken(t *testing.T) {
	q := newFakeQuerier()
	v := uploadtoken.New(q)

	_, err := v.Verify(context.Background(), "eyJhbGciOiJFZERTQSJ9.someJWT.sig")
	require.Error(t, err)
	require.True(t, errors.Is(err, auth.ErrTokenNotForMe), "non-nova_ut_ token should return ErrTokenNotForMe")
}

func TestVerify_NotFound(t *testing.T) {
	q := newFakeQuerier() // empty — no tokens
	// Build a valid-looking nova_ut_ wire
	wire, _, _, err := uploadtoken.Generate()
	require.NoError(t, err)

	v := uploadtoken.New(q)
	_, err = v.Verify(context.Background(), wire)
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrTokenNotForMe, "not-found token should yield 401, not ErrTokenNotForMe")
}
