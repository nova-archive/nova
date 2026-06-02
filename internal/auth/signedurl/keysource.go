package signedurl

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
)

// signingKeyQuerier is the subset of *gen.Queries the KeySource needs.
type signingKeyQuerier interface {
	GetActiveSigningKey(ctx context.Context) (gen.GetActiveSigningKeyRow, error)
	GetSigningKeyByKID(ctx context.Context, kid string) (gen.GetSigningKeyByKIDRow, error)
}

// unwrapper unwraps a wrapped key under the master-key version that wrapped it.
// *envelope.Keystore satisfies it.
type unwrapper interface {
	Unwrap(ctx context.Context, wrapped []byte, versionID uuid.UUID) ([]byte, error)
}

type cachedKey struct {
	key       Key
	expiresAt time.Time
}

// DBKeySource resolves signing keys from signing_keys, unwrapping the HMAC
// secret with the keystore, behind a short TTL cache. It satisfies KeySource
// and additionally exposes Active (for minting) and Invalidate (after rotation).
type DBKeySource struct {
	q   signingKeyQuerier
	ks  unwrapper
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]cachedKey
}

// NewKeySource builds a DBKeySource. A ttl <= 0 disables caching.
func NewKeySource(q signingKeyQuerier, ks unwrapper, ttl time.Duration) *DBKeySource {
	return &DBKeySource{q: q, ks: ks, ttl: ttl, cache: map[string]cachedKey{}}
}

// ByKID returns the unwrapped key for kid when it is usable — state 'active', or
// 'retired' still within its grace window (retire_after in the future).
// Otherwise it returns ErrUnknownKID. Results are cached for ttl.
func (s *DBKeySource) ByKID(ctx context.Context, kid string) (Key, error) {
	now := time.Now()
	s.mu.Lock()
	if c, ok := s.cache[kid]; ok && now.Before(c.expiresAt) {
		s.mu.Unlock()
		return c.key, nil
	}
	s.mu.Unlock()

	row, err := s.q.GetSigningKeyByKID(ctx, kid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrUnknownKID
	}
	if err != nil {
		return Key{}, err
	}
	if !usable(row.State, row.RetireAfter, now) {
		return Key{}, ErrUnknownKID
	}
	secret, err := s.ks.Unwrap(ctx, row.WrappedKey, uuid.UUID(row.MasterKeyVersionID.Bytes))
	if err != nil {
		return Key{}, err
	}
	key := Key{KID: row.Kid, Secret: secret}

	if s.ttl > 0 {
		// Bound the cache entry so a within-grace retired key is never served
		// past its retire_after (whichever is sooner: ttl or grace end).
		exp := now.Add(s.ttl)
		if row.State == gen.KeyStateRetired && row.RetireAfter.Valid && row.RetireAfter.Time.Before(exp) {
			exp = row.RetireAfter.Time
		}
		s.mu.Lock()
		s.cache[kid] = cachedKey{key: key, expiresAt: exp}
		s.mu.Unlock()
	}
	return key, nil
}

// Active returns the current active signing key, unwrapped. Used by minting.
func (s *DBKeySource) Active(ctx context.Context) (Key, error) {
	row, err := s.q.GetActiveSigningKey(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrUnknownKID
	}
	if err != nil {
		return Key{}, err
	}
	secret, err := s.ks.Unwrap(ctx, row.WrappedKey, uuid.UUID(row.MasterKeyVersionID.Bytes))
	if err != nil {
		return Key{}, err
	}
	return Key{KID: row.Kid, Secret: secret}, nil
}

// Invalidate clears the key cache. Called after a rotation so the new active key
// (and the freshly-retired one) are re-resolved on next use.
func (s *DBKeySource) Invalidate() {
	s.mu.Lock()
	s.cache = map[string]cachedKey{}
	s.mu.Unlock()
}

// usable applies the verification key rule.
func usable(state gen.KeyState, retireAfter pgtype.Timestamptz, now time.Time) bool {
	switch state {
	case gen.KeyStateActive:
		return true
	case gen.KeyStateRetired:
		return retireAfter.Valid && retireAfter.Time.After(now)
	default:
		return false
	}
}
