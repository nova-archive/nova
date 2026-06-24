package storage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/tokens"
)

// pgNow returns a pgtype.Timestamptz set to the current time, used as the
// throttle threshold for last_accessed_at touches.
func pgNow() pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

// ErrNoSourceableHolder is returned by OpenBytes on a local cache miss when the
// donor-fetch tier is configured but no sourceable holder could serve verified
// bytes (none advertised, all unreachable, or all returned tampered/oversize
// bytes). Task 8 maps it to HTTP 503 (the blob may exist; we just cannot fetch
// it right now). It is deliberately distinct from ErrBlobNotFound (404).
var ErrNoSourceableHolder = errors.New("storage: no sourceable holder")

// donorFetcher abstracts the network call to a donor read-source endpoint so
// OpenBytes/selectAndFetch are unit-testable with a fake. The real
// implementation (httpDonorFetcher) issues a GET over the coordinator's mTLS
// client identity; tests inject a fake returning canned envelope bytes.
type donorFetcher interface {
	// Fetch issues GET https://<addr>/fed/v1/blob/{cid} with the read grant in
	// the X-Nova-Repair-Token header and returns the ciphertext-envelope body.
	// The caller MUST Close the returned reader.
	Fetch(ctx context.Context, addr, cid, grant string) (io.ReadCloser, error)
}

// donorQuerier is the subset of gen.Queries the donor-fetch tier needs. The
// real *gen.Queries satisfies it; tests inject a pool-free fake. This keeps
// selectAndFetch verifiable without Postgres.
type donorQuerier interface {
	GetEnvelopeSize(ctx context.Context, cid string) (int64, error)
	ListSourceableHolders(ctx context.Context, arg gen.ListSourceableHoldersParams) ([]gen.ListSourceableHoldersRow, error)
	AdmitToCache(ctx context.Context, arg gen.AdmitToCacheParams) error
	TouchLastAccessed(ctx context.Context, arg gen.TouchLastAccessedParams) error
}

// donorReadSource is the coordinator's donor-backed read tier. It is nil unless
// WithDonorReadSource (or the post-construction setter) installs it, in which
// case a local cache miss triggers a verified donor fetch instead of a 404.
type donorReadSource struct {
	fetcher   donorFetcher
	signer    *tokens.Signer
	q         donorQuerier
	ttl       time.Duration
	staleSecs float64
}

// WithDonorReadSource enables the coordinator donor-backed read tier (P2-M4.1).
// On a local cache miss OpenBytes selects a reputation-ordered sourceable
// holder, fetches the ciphertext envelope over clientTLS using a freshly minted
// read grant, VERIFIES it (deterministic re-import → root CID == cid) before
// decrypting, and re-admits it to the local cache. clientTLS and signer must be
// non-nil; a nil/zero option is a no-op (donor-fetch stays disabled and a miss
// returns ErrBlobNotFound — today's behavior).
//
// ttl is the read-grant validity window; staleSecs bounds donor freshness in
// ListSourceableHolders (a holder unseen for longer is not considered).
func WithDonorReadSource(clientTLS *tls.Config, signer *tokens.Signer, ttl time.Duration, staleSecs float64) Option {
	return func(o *svcOpts) {
		if clientTLS == nil || signer == nil {
			return
		}
		o.donorReadSource = &donorReadSource{
			fetcher:   newHTTPDonorFetcher(clientTLS),
			signer:    signer,
			ttl:       ttl,
			staleSecs: staleSecs,
		}
	}
}

// EnableDonorReadSource installs the donor-fetch tier after construction. The
// storage service is built inside coordinator.New, but the coordinator's mTLS
// client identity and the repair-token signer are loaded later in main (after
// the federation block), so the wiring is deferred to a setter that mirrors the
// federation server's SetSourceDeps pattern. nil clientTLS or signer is a no-op
// (graceful degradation: miss → ErrBlobNotFound).
func (s *Service) EnableDonorReadSource(clientTLS *tls.Config, signer *tokens.Signer, ttl time.Duration, staleSecs float64) {
	if clientTLS == nil || signer == nil {
		return
	}
	s.donor = &donorReadSource{
		fetcher:   newHTTPDonorFetcher(clientTLS),
		signer:    signer,
		q:         s.q,
		ttl:       ttl,
		staleSecs: staleSecs,
	}
}

// setDonorReadSourceForTest installs a fully-faked donor tier (fetcher + signer
// + querier) for white-box unit tests. Production callers use
// WithDonorReadSource / EnableDonorReadSource.
func (s *Service) setDonorReadSourceForTest(f donorFetcher, signer *tokens.Signer, q donorQuerier, ttl time.Duration, staleSecs float64) {
	s.donor = &donorReadSource{fetcher: f, signer: signer, q: q, ttl: ttl, staleSecs: staleSecs}
}

// ensureLocal guarantees the blob's bytes are pinned locally before the decrypt
// path runs, or returns a sentinel. On a local hit it touches the cache LRU and
// returns nil. On a miss with no donor tier configured it returns ErrBlobNotFound
// (today's behavior). On a miss with the donor tier configured it fetches and
// verifies from a sourceable holder, returning ErrNoSourceableHolder if none can.
//
// The local decision is based on backend.Has, NOT on a blob_storage_state row:
// legacy / pre-Task-11 blobs have no projection row and must still read. The
// projection-vs-Has reconcile and the origin_copy mode gate are Task 9/12.
func (s *Service) ensureLocal(ctx context.Context, c gocid.Cid, v *BlobView) error {
	has, err := s.backend.Has(ctx, c)
	if err != nil {
		return fmt.Errorf("storage: backend has: %w", err)
	}
	if has {
		slog.Info("storage.read.cache_hit", "cid", v.CID)
		s.touchCache(ctx, v.CID)
		return nil
	}
	slog.Info("storage.read.cache_miss", "cid", v.CID)
	if s.donor == nil {
		// Donor-fetch not configured: preserve the pre-M4.1 not-found behavior.
		return ErrBlobNotFound
	}
	return s.selectAndFetch(ctx, v)
}

// touchCache best-effort bumps last_accessed_at for the cached blob. A blob with
// no blob_storage_state row (legacy / origin) is a no-op (the UPDATE matches no
// rows); errors are swallowed — a read must not fail because the LRU bump did.
func (s *Service) touchCache(ctx context.Context, cidStr string) {
	if s.donor == nil || s.donor.q == nil {
		return
	}
	_ = s.donor.q.TouchLastAccessed(ctx, gen.TouchLastAccessedParams{
		Cid:         cidStr,
		ThresholdAt: pgNow(),
	})
}

// selectAndFetch is the local-miss donor path. It selects reputation-ordered
// sourceable holders, then for each: mints a read grant, fetches the envelope
// bounded by envelope_size, re-imports it (AddDeterministic) to VERIFY the root
// CID equals the assignment cid — the verification gate — and only on a match
// re-admits it to the local cache. Unverified bytes are NEVER decrypted or
// served. Returns nil once a holder serves verified bytes (now pinned locally),
// or ErrNoSourceableHolder if every holder failed.
func (s *Service) selectAndFetch(ctx context.Context, v *BlobView) error {
	d := s.donor
	cidStr := v.CID

	envSize, err := d.q.GetEnvelopeSize(ctx, cidStr)
	if err != nil {
		// No active manifest row ⇒ nothing sourceable to fetch (quarantined,
		// tombstoned, or simply unknown). Treat as not-found rather than 503.
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "envelope_size")
		return ErrBlobNotFound
	}

	holders, err := d.q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{
		Cid:       cidStr,
		StaleSecs: d.staleSecs,
	})
	if err != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "list_holders")
		return ErrNoSourceableHolder
	}
	if len(holders) == 0 {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "no_holders")
		return ErrNoSourceableHolder
	}

	now := time.Now()
	for _, h := range holders {
		addr := h.SourceNebulaAddr.String
		if addr == "" {
			continue
		}
		nodeID := uuid.UUID(h.NodeID.Bytes).String()
		assignmentID := uuid.UUID(h.AssignmentID.Bytes).String()

		grant, gerr := d.signer.MintReadGrant(nodeID, cidStr, assignmentID, h.Generation, envSize, d.ttl, now, now)
		if gerr != nil {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "mint_grant")
			continue
		}

		start := time.Now()
		rc, ferr := d.fetcher.Fetch(ctx, addr, cidStr, grant)
		if ferr != nil {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "fetch")
			continue
		}
		// Bound the read at envelope_size+1 so an oversize body is detected
		// (the extra byte makes len > envSize observable) and never buffered
		// unbounded. The donor's preflight already caps at max_bytes=envSize,
		// but the coordinator does not trust the donor — it bounds locally too.
		body, rerr := io.ReadAll(io.LimitReader(rc, envSize+1))
		rc.Close()
		if rerr != nil {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "read")
			continue
		}
		if int64(len(body)) > envSize {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "oversize")
			continue
		}

		// VERIFY-BEFORE-DECRYPT: deterministically re-import the bytes and
		// require the root CID to equal the assignment cid. AddDeterministic
		// also re-pins locally on success, so a verified blob is immediately
		// readable by the decrypt path below. A mismatch discards the bytes
		// and advances to the next holder; unverified bytes are never served.
		add, aerr := s.backend.AddDeterministic(ctx, body)
		if aerr != nil {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "import")
			continue
		}
		if add.CID.String() != cidStr {
			slog.Warn("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID,
				"reason", "cid_mismatch", "got_cid", add.CID.String())
			continue
		}

		// Verified + pinned. Best-effort re-admit to the local cache as a
		// probationary cache entry (durability_class=cache). A failed admit
		// does not fail the read — the bytes are already pinned and serveable.
		if aerr := d.q.AdmitToCache(ctx, gen.AdmitToCacheParams{
			Cid:             cidStr,
			DurabilityClass: "cache",
			LocalBytes:      envSize,
		}); aerr != nil {
			slog.Warn("storage.read.admit_failed", "cid", cidStr, "err", aerr)
		}

		slog.Info("storage.read.donor_fetch", "cid", cidStr, "holder", nodeID,
			"bytes", len(body), "dur_ms", time.Since(start).Milliseconds())
		return nil
	}

	slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "all_holders_failed")
	return ErrNoSourceableHolder
}

// httpDonorFetcher is the production donorFetcher: an mTLS GET to the donor's
// read-source endpoint presenting the coordinator's nova://coordinator/<uuid>
// client cert.
type httpDonorFetcher struct {
	hc *http.Client
}

func newHTTPDonorFetcher(clientTLS *tls.Config) *httpDonorFetcher {
	return &httpDonorFetcher{
		hc: &http.Client{
			Transport: &http.Transport{TLSClientConfig: clientTLS},
		},
	}
}

func (f *httpDonorFetcher) Fetch(ctx context.Context, addr, cidStr, grant string) (io.ReadCloser, error) {
	u := "https://" + addr + "/fed/v1/blob/" + url.PathEscape(cidStr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Nova-Repair-Token", grant)
	resp, err := f.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("donor fetch %s: status %d", cidStr, resp.StatusCode)
	}
	return resp.Body, nil
}
