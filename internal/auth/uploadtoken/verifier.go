package uploadtoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/db/gen"
)

// Querier is the minimal DB interface the Verifier needs. *gen.Queries satisfies
// this structurally; tests can supply a lightweight fake.
type Querier interface {
	GetUploadTokenByID(ctx context.Context, id pgtype.UUID) (gen.UploadToken, error)
	TouchUploadTokenUsed(ctx context.Context, id pgtype.UUID) error
}

// Verifier implements auth.Verifier for nova_ut_ scoped upload tokens.
type Verifier struct {
	q Querier
}

// New constructs an upload-token Verifier backed by q.
func New(q Querier) *Verifier {
	return &Verifier{q: q}
}

// Verify validates a raw bearer token. It returns:
//
//   - auth.ErrTokenNotForMe if the raw string is not a nova_ut_ token
//     (so chained middleware tries the next verifier).
//   - A plain error (→ 401) for any other failure: not found, revoked,
//     expired, or wrong secret.
//   - auth.Identity on success.
func (v *Verifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	id, secret, err := ParseWire(raw)
	if errors.Is(err, ErrNotUploadToken) {
		return auth.Identity{}, auth.ErrTokenNotForMe
	}
	if err != nil {
		// Mine but malformed → 401.
		return auth.Identity{}, fmt.Errorf("uploadtoken: parse wire: %w", err)
	}

	pgID := pgtype.UUID{Bytes: id, Valid: true}
	row, err := v.q.GetUploadTokenByID(ctx, pgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Identity{}, fmt.Errorf("uploadtoken: token not found")
		}
		return auth.Identity{}, fmt.Errorf("uploadtoken: db lookup: %w", err)
	}

	if row.RevokedAt.Valid {
		return auth.Identity{}, fmt.Errorf("uploadtoken: token has been revoked")
	}

	if row.ExpiresAt.Valid && row.ExpiresAt.Time.Before(time.Now()) {
		return auth.Identity{}, fmt.Errorf("uploadtoken: token has expired")
	}

	if !EqualHash(HashSecret(secret), row.TokenHash) {
		return auth.Identity{}, fmt.Errorf("uploadtoken: invalid token secret")
	}

	// Best-effort: update last_used_at; ignore errors.
	_ = v.q.TouchUploadTokenUsed(ctx, pgID)

	createdBy := uuid.UUID(row.CreatedBy.Bytes).String()
	return auth.Identity{
		UserID:       createdBy,
		Role:         string(row.Role),
		CredentialID: id.String(),
		Issuer:       "nova:upload-token",
	}, nil
}
