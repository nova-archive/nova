package signedurl

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
)

// revocationQuerier is the subset of *gen.Queries DBRevocations needs.
type revocationQuerier interface {
	ListRevocations(ctx context.Context) ([]gen.ListRevocationsRow, error)
}

type revSnapshot struct {
	cid, aud, kid map[string]struct{}
	prefixes      []string
}

func emptySnapshot() *revSnapshot {
	return &revSnapshot{cid: map[string]struct{}{}, aud: map[string]struct{}{}, kid: map[string]struct{}{}}
}

// DBRevocations is an in-memory, atomically-swapped view of
// signed_url_revocations. It satisfies Revocations. Load it once at startup,
// Refresh on a ticker, and Invalidate immediately after a revoke writes a row.
// (Single-node Phase 1: invalidation is in-process; cross-node fan-out is
// Phase 2.)
type DBRevocations struct {
	q    revocationQuerier
	snap atomic.Pointer[revSnapshot]
}

// NewRevocations builds an (empty) DBRevocations. Call Load before serving.
func NewRevocations(q revocationQuerier) *DBRevocations {
	r := &DBRevocations{q: q}
	r.snap.Store(emptySnapshot())
	return r
}

// Load reads signed_url_revocations and atomically replaces the snapshot.
func (r *DBRevocations) Load(ctx context.Context) error {
	rows, err := r.q.ListRevocations(ctx)
	if err != nil {
		return err
	}
	s := emptySnapshot()
	for _, row := range rows {
		switch row.Kind {
		case "cid":
			s.cid[row.Value] = struct{}{}
		case "aud":
			s.aud[row.Value] = struct{}{}
		case "kid":
			s.kid[row.Value] = struct{}{}
		case "path_prefix":
			s.prefixes = append(s.prefixes, row.Value)
		}
	}
	r.snap.Store(s)
	return nil
}

// Invalidate reloads the snapshot immediately (called after a revoke).
func (r *DBRevocations) Invalidate(ctx context.Context) error { return r.Load(ctx) }

// RefreshEvery reloads the snapshot on interval until ctx is done. Run in a
// goroutine; a load error leaves the prior snapshot in place.
func (r *DBRevocations) RefreshEvery(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = r.Load(ctx)
		}
	}
}

// IsRevoked reports whether any parsed field of a signed URL is revoked.
func (r *DBRevocations) IsRevoked(cid, aud, kid, path string) bool {
	s := r.snap.Load()
	if cid != "" {
		if _, ok := s.cid[cid]; ok {
			return true
		}
	}
	if _, ok := s.aud[aud]; ok {
		return true
	}
	if _, ok := s.kid[kid]; ok {
		return true
	}
	for _, p := range s.prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
