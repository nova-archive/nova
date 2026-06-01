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

// errRefreshInternal indicates a transient/internal failure during refresh
// rotation (DB error during a critical operation, family-revoke retries
// exhausted). Handlers map this to 503 with a Retry-After hint rather than
// 401, so a real outage is distinguishable from a token rejection. M6.2 B4
// added this distinction so a failed family-revoke during reuse detection
// surfaces clearly instead of silently returning 401 while parallel
// attackers continue to succeed against the unrevoked family.
var errRefreshInternal = errors.New("localissuer: refresh internal error")

// Retry knobs for the family-revoke critical path. Two attempts + 100 ms
// gap absorbs typical Postgres restart blips while keeping the user-facing
// latency tight. Variables (not constants) so tests can dial them down.
var (
	revokeFamilyMaxAttempts = 2
	revokeFamilyRetryDelay  = 100 * time.Millisecond
)

// retryUntil runs op up to maxAttempts times, sleeping `delay` between
// attempts. ctx cancellation aborts the retry early (returning ctx.Err).
// On all-attempts-fail, returns the last op error. Helper kept generic
// so the same retry policy can wrap other critical DB ops if needed.
func retryUntil(ctx context.Context, op func() error, maxAttempts int, delay time.Duration) error {
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		if err := op(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

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

// rotate validates raw, checks for reuse / expiry / disabled owner, mints a
// replacement token, and uses a conditional UPDATE as the serialization point
// to prevent concurrent double-spend. The sequence is:
//
//  1. Look up the presented token; reject (no-rows) unknowns.
//  2. Reject reused tokens (rotated_to or revoked_at already set) with family
//     revocation — this limits blast radius of a stolen refresh token.
//  3. Reject expired tokens.
//  4. Reject tokens owned by a disabled user; revoke the whole family.
//  5. Mint and INSERT the child token.
//  6. Conditionally mark the old token rotated (WHERE rotated_to IS NULL AND
//     revoked_at IS NULL). If rows-affected == 0 another concurrent call won
//     the race: revoke the just-minted child (no orphan left live) and return
//     errRefreshInvalid.
//
// Only when exactly one row is marked does rotate() return the new raw token.
func (rs *refreshStore) rotate(ctx context.Context, raw string, ua string) (uuid.UUID, string, error) {
	row, err := rs.q.GetRefreshTokenByHash(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, "", errRefreshInvalid
		}
		slog.Error("refresh: db error on hash lookup", "err", err)
		return uuid.Nil, "", errRefreshInternal
	}

	// Reuse detection: token was already rotated or explicitly revoked. The
	// family-revoke is the load-bearing security action here — if it doesn't
	// succeed, parallel attackers holding sibling tokens from the same family
	// would continue to succeed. Retry on transient DB error; if all
	// attempts fail, surface as errRefreshInternal (→ 503 Retry-After) so
	// the failure is visible instead of silently returning 401 while the
	// family stays live.
	if row.RevokedAt.Valid || row.RotatedTo.Valid {
		err := retryUntil(ctx, func() error {
			return rs.q.RevokeRefreshTokenFamily(ctx, row.UserID)
		}, revokeFamilyMaxAttempts, revokeFamilyRetryDelay)
		if err != nil {
			slog.Error("refresh family revoke failed (reuse-detection critical path)",
				"user_id", uuid.UUID(row.UserID.Bytes), "err", err)
			return uuid.Nil, "", errRefreshInternal
		}
		slog.Warn("refresh token reuse detected", "user_id", uuid.UUID(row.UserID.Bytes))
		return uuid.Nil, "", errRefreshInvalid
	}

	// Expiry check. Token is already expired; the per-row revoke is bookkeeping,
	// not security-critical (the next presentation will hit the reuse-detection
	// branch above and family-revoke from there).
	if row.ExpiresAt.Before(time.Now()) {
		if err := rs.q.RevokeRefreshToken(ctx, row.ID); err != nil {
			slog.Error("refresh token revoke failed", "user_id", uuid.UUID(row.UserID.Bytes), "err", err)
		}
		return uuid.Nil, "", errRefreshInvalid
	}

	// Disabled-user check: a disabled user must not be able to mint new tokens.
	// Family revoke is best-effort here — the user.Disabled check above
	// continues to block subsequent refreshes even if the rows are not
	// explicitly revoked, so a single failed revoke is not a security gap.
	user, err := rs.q.GetUserByID(ctx, row.UserID)
	if err != nil {
		slog.Error("refresh: db error on user lookup", "err", err)
		return uuid.Nil, "", errRefreshInternal
	}
	if user.Disabled {
		if err := rs.q.RevokeRefreshTokenFamily(ctx, row.UserID); err != nil {
			slog.Error("refresh family revoke failed (disabled user)", "user_id", uuid.UUID(row.UserID.Bytes), "err", err)
		}
		return uuid.Nil, "", errRefreshInvalid
	}

	// Happy path: mint replacement and attempt to claim the old token slot.
	newRaw, newHash, err := mint()
	if err != nil {
		return uuid.Nil, "", errRefreshInternal
	}
	newID, err := rs.q.InsertRefreshToken(ctx, gen.InsertRefreshTokenParams{
		UserID:    row.UserID,
		TokenHash: newHash,
		ExpiresAt: time.Now().Add(rs.ttl),
		UserAgent: pgText(ua),
	})
	if err != nil {
		slog.Error("refresh: db error inserting child token", "err", err)
		return uuid.Nil, "", errRefreshInternal
	}

	// Serialization point: conditional UPDATE ensures exactly one concurrent
	// caller can mark this token rotated. If we get rowsAffected == 0 another
	// goroutine/process already rotated or revoked this token; revoke our
	// just-inserted child so it doesn't remain live, then signal failure.
	rowsAffected, err := rs.q.MarkRefreshTokenRotated(ctx, gen.MarkRefreshTokenRotatedParams{
		ID:        row.ID,
		RotatedTo: newID,
	})
	if err != nil {
		slog.Error("refresh: db error marking rotated", "err", err)
		return uuid.Nil, "", errRefreshInternal
	}
	if rowsAffected == 0 {
		// Lost the race — revoke the orphaned child we just minted.
		if err := rs.q.RevokeRefreshToken(ctx, newID); err != nil {
			slog.Error("orphan child token revoke failed", "user_id", uuid.UUID(row.UserID.Bytes), "err", err)
		}
		return uuid.Nil, "", errRefreshInvalid
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
