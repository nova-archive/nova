package localissuer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// errRefreshInvalid is the single opaque error returned for all refresh token
// failure modes — expired, revoked, rotated, or unknown. Handlers map this to
// a generic 401 so callers learn nothing about which check failed.
var errRefreshInvalid = errors.New("localissuer: refresh token invalid")

// refreshStore holds the query handle and the token TTL used when issuing.
type refreshStore struct {
	q   *gen.Queries
	ttl time.Duration
}

// newRefreshStore constructs a refreshStore backed by the given query handle.
func newRefreshStore(q *gen.Queries, ttl time.Duration) *refreshStore {
	return &refreshStore{q: q, ttl: ttl}
}

// mint generates a cryptographically random opaque token and its SHA-256 hash.
// The raw string is safe to send over the wire; only the hash is persisted.
func mint() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, sum[:], nil
}

// hashToken returns the SHA-256 hash of a raw token string.
func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// pgUUID converts a google/uuid.UUID to the pgtype representation.
func pgUUID(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

// pgText returns a valid pgtype.Text when s is non-empty, zero value otherwise.
func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// issue mints a new refresh token for userID, persists its hash, and returns
// the raw opaque string that must be delivered to the client.
func (rs *refreshStore) issue(ctx context.Context, userID uuid.UUID, ua string) (string, error) {
	raw, hash, err := mint()
	if err != nil {
		return "", err
	}
	_, err = rs.q.InsertRefreshToken(ctx, gen.InsertRefreshTokenParams{
		UserID:    pgUUID(userID),
		TokenHash: hash,
		ExpiresAt: time.Now().Add(rs.ttl),
		UserAgent: pgText(ua),
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// rotate validates raw, checks for reuse / expiry, mints a replacement token,
// atomically marks the old token as rotated, and returns the owner's UUID plus
// the new raw token.
//
// Reuse detection: if the presented token has already been rotated or revoked,
// the entire token family is revoked immediately and errRefreshInvalid is
// returned. This limits the blast radius of a stolen refresh token.
func (rs *refreshStore) rotate(ctx context.Context, raw string, ua string) (uuid.UUID, string, error) {
	row, err := rs.q.GetRefreshTokenByHash(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", errRefreshInvalid
		}
		return uuid.Nil, "", err
	}

	// Reuse detection: token was already rotated or explicitly revoked.
	if row.RevokedAt.Valid || row.RotatedTo.Valid {
		_ = rs.q.RevokeRefreshTokenFamily(ctx, row.UserID)
		slog.Warn("refresh token reuse detected", "user_id", uuid.UUID(row.UserID.Bytes))
		return uuid.Nil, "", errRefreshInvalid
	}

	// Expiry check.
	if row.ExpiresAt.Before(time.Now()) {
		_ = rs.q.RevokeRefreshToken(ctx, row.ID)
		return uuid.Nil, "", errRefreshInvalid
	}

	// Happy path: mint replacement and link it to the old token.
	newRaw, newHash, err := mint()
	if err != nil {
		return uuid.Nil, "", err
	}
	newID, err := rs.q.InsertRefreshToken(ctx, gen.InsertRefreshTokenParams{
		UserID:    row.UserID,
		TokenHash: newHash,
		ExpiresAt: time.Now().Add(rs.ttl),
		UserAgent: pgText(ua),
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	if err := rs.q.MarkRefreshTokenRotated(ctx, gen.MarkRefreshTokenRotatedParams{
		ID:        row.ID,
		RotatedTo: newID,
	}); err != nil {
		return uuid.Nil, "", err
	}

	return uuid.UUID(row.UserID.Bytes), newRaw, nil
}

// revoke looks up the token by hash and marks it revoked. If the token is not
// found the call is a no-op (idempotent logout).
func (rs *refreshStore) revoke(ctx context.Context, raw string) error {
	row, err := rs.q.GetRefreshTokenByHash(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already gone — treat as success
		}
		return err
	}
	return rs.q.RevokeRefreshToken(ctx, row.ID)
}
