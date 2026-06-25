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
	"sync"
	"time"

	"github.com/google/uuid"
	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

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

// ReadSourceConfig carries the donor read-tier tunables. It is passed once at
// Enable/With time (not per-call) so the shared containment state — bulkhead,
// per-donor limits, breaker map, single-flight group — is constructed exactly
// once. Zero values are normalized to documented defaults by withDefaults so a
// partially-filled struct (e.g. from a config block) is still safe.
type ReadSourceConfig struct {
	TTL              time.Duration // read-grant validity window
	StaleSecs        float64       // donor-freshness bound for ListSourceableHolders
	Timeout          time.Duration // per-holder fetch+read timeout
	Bulkhead         int64         // coordinator-wide max concurrent donor fetches
	PerDonorLimit    int64         // max concurrent fetches to a single donor addr
	BreakerThreshold int           // consecutive failures before a donor breaker opens
	BreakerCooldown  time.Duration // half-open delay after the breaker opens
	MaxFallbacks     int           // max donor fetch ATTEMPTS per request
}

// Documented defaults for the containment knobs. These mirror the accessor
// defaults on config.Federation; both must stay in sync.
const (
	defaultReadSourceTimeout          = 30 * time.Second
	defaultReadSourceBulkhead         = 16
	defaultReadSourcePerDonorLimit    = 4
	defaultReadSourceBreakerThreshold = 5
	defaultReadSourceBreakerCooldown  = 30 * time.Second
	defaultReadSourceMaxFallbacks     = 3
)

// withDefaults normalizes non-positive fields to the documented defaults so a
// caller may leave any knob zero. TTL/StaleSecs are passed through unchanged
// (their own defaults are applied upstream in config.Federation accessors).
func (c ReadSourceConfig) withDefaults() ReadSourceConfig {
	if c.Timeout <= 0 {
		c.Timeout = defaultReadSourceTimeout
	}
	if c.Bulkhead <= 0 {
		c.Bulkhead = defaultReadSourceBulkhead
	}
	if c.PerDonorLimit <= 0 {
		c.PerDonorLimit = defaultReadSourcePerDonorLimit
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = defaultReadSourceBreakerThreshold
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = defaultReadSourceBreakerCooldown
	}
	if c.MaxFallbacks <= 0 {
		c.MaxFallbacks = defaultReadSourceMaxFallbacks
	}
	return c
}

// donorBreaker is a per-donor circuit breaker. After threshold consecutive
// failures it opens; while open every attempt is SKIPPED (no Fetch) until
// cooldown elapses, after which a single half-open trial is allowed — success
// resets, failure reopens. It is keyed by the dialed donor addr.
type donorBreaker struct {
	failures int       // consecutive failures
	openedAt time.Time // when the breaker last opened (zero ⇒ closed)
}

// donorReadSource is the coordinator's donor-backed read tier. It is nil unless
// WithDonorReadSource (or the post-construction setter) installs it, in which
// case a local cache miss triggers a verified donor fetch instead of a 404.
//
// The containment state (sf, bulkhead, perDonor, breakers + bmu) is SHARED
// across all requests: it is constructed once when the tier is installed and is
// safe for concurrent use. donorReadSource is otherwise read-only after install.
type donorReadSource struct {
	fetcher donorFetcher
	signer  *tokens.Signer
	q       donorQuerier
	cfg     ReadSourceConfig

	sf       *singleflight.Group            // collapse concurrent misses by cid
	bulkhead *semaphore.Weighted            // coordinator-wide donor-fetch bound
	pdMu     sync.Mutex                     // guards perDonor
	perDonor map[string]*semaphore.Weighted // addr -> per-donor concurrency bound
	bmu      sync.Mutex                     // guards breakers
	breakers map[string]*donorBreaker       // addr -> circuit breaker
}

// newDonorReadSource builds the tier with its shared containment state. cfg is
// normalized via withDefaults so callers may leave knobs zero.
func newDonorReadSource(fetcher donorFetcher, signer *tokens.Signer, q donorQuerier, cfg ReadSourceConfig) *donorReadSource {
	cfg = cfg.withDefaults()
	return &donorReadSource{
		fetcher:  fetcher,
		signer:   signer,
		q:        q,
		cfg:      cfg,
		sf:       &singleflight.Group{},
		bulkhead: semaphore.NewWeighted(cfg.Bulkhead),
		perDonor: map[string]*semaphore.Weighted{},
		breakers: map[string]*donorBreaker{},
	}
}

// perDonorSem returns the shared per-donor semaphore for addr, creating it on
// first use. Concurrent fetches to the same donor are bounded by PerDonorLimit.
func (d *donorReadSource) perDonorSem(addr string) *semaphore.Weighted {
	d.pdMu.Lock()
	defer d.pdMu.Unlock()
	s, ok := d.perDonor[addr]
	if !ok {
		s = semaphore.NewWeighted(d.cfg.PerDonorLimit)
		d.perDonor[addr] = s
	}
	return s
}

// breakerOpen reports whether addr's breaker is currently open (and the holder
// must be skipped). A breaker past its cooldown transitions to half-open and is
// reported closed so exactly one trial fetch is allowed.
func (d *donorReadSource) breakerOpen(addr string, now time.Time) bool {
	d.bmu.Lock()
	defer d.bmu.Unlock()
	b := d.breakers[addr]
	if b == nil || b.openedAt.IsZero() {
		return false
	}
	if now.Sub(b.openedAt) >= d.cfg.BreakerCooldown {
		// Half-open: allow a single trial. Keep openedAt zero so this trial is
		// not itself skipped; recordFailure will reopen on failure.
		b.openedAt = time.Time{}
		return false
	}
	return true
}

// recordFailure bumps addr's consecutive-failure count and opens the breaker at
// the threshold. Called for every failed attempt (fetch error, read error,
// timeout, oversize, import error, cid mismatch).
func (d *donorReadSource) recordFailure(addr string, now time.Time) {
	d.bmu.Lock()
	defer d.bmu.Unlock()
	b := d.breakers[addr]
	if b == nil {
		b = &donorBreaker{}
		d.breakers[addr] = b
	}
	b.failures++
	if b.failures >= d.cfg.BreakerThreshold && b.openedAt.IsZero() {
		b.openedAt = now
	}
}

// recordSuccess clears addr's breaker after a verified fetch.
func (d *donorReadSource) recordSuccess(addr string) {
	d.bmu.Lock()
	defer d.bmu.Unlock()
	if b := d.breakers[addr]; b != nil {
		b.failures = 0
		b.openedAt = time.Time{}
	}
}

// WithDonorReadSource enables the coordinator donor-backed read tier (P2-M4.1).
// On a local cache miss OpenBytes selects a reputation-ordered sourceable
// holder, fetches the ciphertext envelope over clientTLS using a freshly minted
// read grant, VERIFIES it (deterministic re-import → root CID == cid) before
// decrypting, and re-admits it to the local cache. clientTLS and signer must be
// non-nil; a nil/zero option is a no-op (donor-fetch stays disabled and a miss
// returns ErrBlobNotFound — today's behavior).
//
// cfg carries the read-grant TTL, donor-freshness window, and the read-path
// containment knobs (per-fetch timeout, bulkhead, per-donor limit, breaker,
// bounded fallback). The shared containment state is built once here.
func WithDonorReadSource(clientTLS *tls.Config, signer *tokens.Signer, cfg ReadSourceConfig) Option {
	return func(o *svcOpts) {
		if clientTLS == nil || signer == nil {
			return
		}
		o.donorReadSource = newDonorReadSource(newHTTPDonorFetcher(clientTLS), signer, nil, cfg)
	}
}

// EnableDonorReadSource installs the donor-fetch tier after construction. The
// storage service is built inside coordinator.New, but the coordinator's mTLS
// client identity and the repair-token signer are loaded later in main (after
// the federation block), so the wiring is deferred to a setter that mirrors the
// federation server's SetSourceDeps pattern. nil clientTLS or signer is a no-op
// (graceful degradation: miss → ErrBlobNotFound).
func (s *Service) EnableDonorReadSource(clientTLS *tls.Config, signer *tokens.Signer, cfg ReadSourceConfig) {
	if clientTLS == nil || signer == nil {
		return
	}
	s.donor = newDonorReadSource(newHTTPDonorFetcher(clientTLS), signer, s.q, cfg)
}

// setDonorReadSourceForTest installs a fully-faked donor tier (fetcher + signer
// + querier) for white-box unit tests, with permissive containment defaults so
// existing single-holder tests are unaffected. Production callers use
// WithDonorReadSource / EnableDonorReadSource.
func (s *Service) setDonorReadSourceForTest(f donorFetcher, signer *tokens.Signer, q donorQuerier, ttl time.Duration, staleSecs float64) {
	s.donor = newDonorReadSource(f, signer, q, ReadSourceConfig{TTL: ttl, StaleSecs: staleSecs})
}

// setDonorReadSourceCfgForTest installs a faked donor tier with an explicit
// containment config, for the single-flight / breaker / fallback-bound tests.
func (s *Service) setDonorReadSourceCfgForTest(f donorFetcher, signer *tokens.Signer, q donorQuerier, cfg ReadSourceConfig) {
	s.donor = newDonorReadSource(f, signer, q, cfg)
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
		// When the coordinator_storage_mode policy is installed, a hit drives the
		// SLRU promote-on-second-access (throttled touch + throttled promote);
		// otherwise fall back to the legacy throttled LRU touch.
		if s.cache != nil {
			s.cache.onHit(ctx, v.CID)
		} else {
			s.touchCache(ctx, v.CID)
		}
		return nil
	}
	slog.Info("storage.read.cache_miss", "cid", v.CID)
	if s.donor == nil {
		// Donor-fetch not configured: preserve the pre-M4.1 not-found behavior.
		return ErrBlobNotFound
	}
	// Single-flight the MISS path keyed by cid: N concurrent misses for one CID
	// run selectAndFetch exactly once; the winner pins locally and verifies, and
	// every caller shares that single result. OpenBytes then proceeds to
	// backend.Get on the now-pinned bytes. Local hits above are NOT collapsed.
	// (Shared callers share the winner's ctx for M4.1 — acceptable per the plan.)
	_, err, _ = s.donor.sf.Do(v.CID, func() (any, error) {
		return nil, s.selectAndFetch(ctx, v)
	})
	return err
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
		StaleSecs: d.cfg.StaleSecs,
	})
	if err != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "list_holders")
		return ErrNoSourceableHolder
	}
	if len(holders) == 0 {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "no_holders")
		return ErrNoSourceableHolder
	}

	// Bulkhead: bound coordinator-wide concurrent donor-fetch work. Only one
	// goroutine per cid reaches here (single-flight), so a straight acquire/
	// defer-release cannot deadlock against the per-donor semaphore below.
	if err := d.bulkhead.Acquire(ctx, 1); err != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "bulkhead")
		return ErrNoSourceableHolder
	}
	defer d.bulkhead.Release(1)

	attempts := 0
	for _, h := range holders {
		addr := h.SourceNebulaAddr.String
		if addr == "" {
			continue
		}
		nodeID := uuid.UUID(h.NodeID.Bytes).String()

		// Circuit breaker: skip a holder whose breaker is open. A SKIP is cheap
		// and does NOT consume a fallback — only real fetch ATTEMPTS are bounded.
		if d.breakerOpen(addr, time.Now()) {
			slog.Info("storage.read.donor_breaker_skip", "cid", cidStr, "holder", nodeID)
			continue
		}

		// Bound the number of actual fetch attempts per request.
		if attempts >= d.cfg.MaxFallbacks {
			slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "fallback_exhausted")
			break
		}
		attempts++

		if err := s.attemptHolder(ctx, d, v, envSize, h, addr, nodeID); err != nil {
			d.recordFailure(addr, time.Now())
			continue
		}
		d.recordSuccess(addr)
		return nil
	}

	slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "reason", "all_holders_failed")
	return ErrNoSourceableHolder
}

// attemptHolder runs a single bounded fetch+verify+admit against one holder. It
// returns nil only when the holder served bytes that VERIFY (re-imported root
// CID == assignment cid) and are now pinned locally; any failure (mint, fetch,
// timeout, read, oversize, import, cid mismatch) returns a non-nil error so the
// caller records the breaker and advances. The verify-before-decrypt gate is
// unchanged. A per-fetch timeout derived from the request ctx caps the attempt;
// a per-donor semaphore bounds concurrent fetches to the same addr.
func (s *Service) attemptHolder(ctx context.Context, d *donorReadSource, v *BlobView, envSize int64, h gen.ListSourceableHoldersRow, addr, nodeID string) error {
	cidStr := v.CID
	assignmentID := uuid.UUID(h.AssignmentID.Bytes).String()
	now := time.Now()

	grant, gerr := d.signer.MintReadGrant(nodeID, cidStr, assignmentID, h.Generation, envSize, d.cfg.TTL, now, now)
	if gerr != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "mint_grant")
		return gerr
	}

	// Per-donor concurrency bound. Acquire under the per-fetch timeout so a
	// saturated donor cannot stall the request indefinitely.
	pdSem := d.perDonorSem(addr)
	actx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
	defer cancel()
	if err := pdSem.Acquire(actx, 1); err != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "per_donor_limit")
		return err
	}
	defer pdSem.Release(1)

	start := time.Now()
	rc, ferr := d.fetcher.Fetch(actx, addr, cidStr, grant)
	if ferr != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "fetch")
		return ferr
	}
	// Bound the read at envelope_size+1 so an oversize body is detected (the
	// extra byte makes len > envSize observable) and never buffered unbounded.
	// The donor's preflight already caps at max_bytes=envSize, but the
	// coordinator does not trust the donor — it bounds locally too.
	body, rerr := io.ReadAll(io.LimitReader(rc, envSize+1))
	rc.Close()
	if rerr != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "read")
		return rerr
	}
	if int64(len(body)) > envSize {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "oversize")
		return errors.New("donor body oversize")
	}

	// VERIFY-BEFORE-DECRYPT: deterministically re-import the bytes and require
	// the root CID to equal the assignment cid. AddDeterministic also re-pins
	// locally on success, so a verified blob is immediately readable by the
	// decrypt path. A mismatch discards the bytes; unverified bytes are never
	// served. (Use the parent ctx for the import so a slow fetch's timeout does
	// not abort the pin of bytes already in hand.)
	add, aerr := s.backend.AddDeterministic(ctx, body)
	if aerr != nil {
		slog.Info("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID, "reason", "import")
		return aerr
	}
	if add.CID.String() != cidStr {
		slog.Warn("storage.read.donor_fetch_failed", "cid", cidStr, "holder", nodeID,
			"reason", "cid_mismatch", "got_cid", add.CID.String())
		return errors.New("donor cid mismatch")
	}

	// Verified + pinned. Hand off to the coordinator_storage_mode policy when
	// installed: bounded_cache admits to probationary and enforces the SLRU byte
	// budget; origin_copy admits without eviction; transient holds nothing. When
	// no cache policy is installed (legacy / DB-free donor unit tests), fall back
	// to the donor tier's direct AdmitToCache so behavior is unchanged. A failed
	// admit/evict does not fail the read — the bytes are already pinned and
	// serveable.
	if s.cache != nil {
		s.cache.admit(ctx, cidStr, envSize)
	} else if aerr := d.q.AdmitToCache(ctx, gen.AdmitToCacheParams{
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
