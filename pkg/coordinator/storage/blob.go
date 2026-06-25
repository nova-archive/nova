package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// Service is the storage core (read + write). It is safe for concurrent use.
type Service struct {
	q             *gen.Queries
	pool          *pgxpool.Pool
	backend       ipfs.Backend
	ks            *envelope.Keystore
	assemblyInUse atomic.Int64
	assemblyLimit atomic.Int64 // <=0 ⇒ unbounded
	maxUploadSize atomic.Int64 // <=0 ⇒ unbounded
	hook          WriteHook

	// donor is the P2-M4.1 donor-backed read tier. nil ⇒ a local cache miss
	// returns ErrBlobNotFound (pre-M4.1 behavior); non-nil ⇒ a miss triggers a
	// verified donor fetch. Installed via WithDonorReadSource or
	// EnableDonorReadSource. Set once at construction/boot; read-only thereafter.
	donor *donorReadSource

	// cache is the P2-M4.1 coordinator_storage_mode policy (size-aware SLRU/2Q
	// bounded cache + transient unpin-on-close). nil ⇒ legacy behavior:
	// donor-fetched bytes are admitted via the donor tier's AdmitToCache and
	// kept forever (origin_copy semantics), and reads do not unpin. Installed
	// via WithStorageMode. Set once at construction; read-only thereafter.
	cache *cachePolicy
}

// Option configures a Service. Read-only callers pass none.
type Option func(*svcOpts)

type svcOpts struct {
	maxUploadSize   int64
	assemblySize    int
	hook            WriteHook
	donorReadSource *donorReadSource
	storageMode     *StorageModeConfig
}

// WithWriteLimits sets the upload size ceiling (bytes) and the maximum number
// of concurrent in-memory assembly operations (the V1-envelope RAM bound).
// Non-positive values keep the defaults (100 MiB / 8).
func WithWriteLimits(maxUploadSize int64, maxConcurrentAssembly int) Option {
	return func(o *svcOpts) {
		if maxUploadSize > 0 {
			o.maxUploadSize = maxUploadSize
		}
		if maxConcurrentAssembly > 0 {
			o.assemblySize = maxConcurrentAssembly
		}
	}
}

// WithProductHook injects the product seam Put calls after the MIME floor and
// before encryption. nil (the default) means no product analysis (raw write path).
func WithProductHook(h WriteHook) Option {
	return func(o *svcOpts) { o.hook = h }
}

// WithStorageMode installs the P2-M4.1 coordinator_storage_mode policy: a
// size-aware SLRU/2Q bounded cache (bounded_cache), an unbounded full-copy
// (origin_copy, the default), or transient unpin-on-read. Unlike the donor tier
// (whose mTLS material is loaded post-construction), the mode config is known at
// construction, so an Option is the right seam. The cache policy uses the real
// SQL projection (*gen.Queries) for SLRU correctness. A zero/empty mode
// normalizes to origin_copy.
func WithStorageMode(cfg StorageModeConfig) Option {
	return func(o *svcOpts) {
		c := cfg
		o.storageMode = &c
	}
}

// NewService builds a storage service over the given pool, IPFS backend, and
// keystore. backend and ks may be nil in tests that exercise Resolve only.
// Write limits default to 100 MiB / 8 concurrent assemblies; override via
// WithWriteLimits. Existing read-only call sites pass no options.
func NewService(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, opts ...Option) *Service {
	o := svcOpts{maxUploadSize: 104857600, assemblySize: 8}
	for _, fn := range opts {
		fn(&o)
	}
	s := &Service{
		q:       gen.New(pool),
		pool:    pool,
		backend: backend,
		ks:      ks,
		hook:    o.hook,
	}
	if o.donorReadSource != nil {
		o.donorReadSource.q = s.q
		s.donor = o.donorReadSource
	}
	if o.storageMode != nil {
		s.cache = newCachePolicyFor(s.q, backend, *o.storageMode)
	}
	s.assemblyLimit.Store(int64(o.assemblySize))
	s.maxUploadSize.Store(o.maxUploadSize)
	return s
}

// tryAcquireAssembly reserves one assembly slot (non-blocking). release frees it.
func (s *Service) tryAcquireAssembly() (release func(), ok bool) {
	limit := s.assemblyLimit.Load()
	if limit <= 0 { // unbounded
		return func() {}, true
	}
	if s.assemblyInUse.Add(1) > limit {
		s.assemblyInUse.Add(-1)
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { s.assemblyInUse.Add(-1) }) }, true
}

// SetWriteLimits updates the live upload-size ceiling and assembly concurrency.
func (s *Service) SetWriteLimits(maxUploadSize int64, maxConcurrentAssembly int) {
	s.maxUploadSize.Store(maxUploadSize)
	s.assemblyLimit.Store(int64(maxConcurrentAssembly))
}

// MaxUploadSize returns the live ceiling (0 ⇒ unbounded).
func (s *Service) MaxUploadSize() int64 { return s.maxUploadSize.Load() }

// Resolve loads and authorizes a blob for anonymous read. It performs no
// Kubo I/O and no decryption. Returns one of the package sentinels on a
// domain failure, or a wrapped error on infrastructure failure (→ 500).
func (s *Service) Resolve(ctx context.Context, cidStr string) (*BlobView, error) {
	if _, err := cid.Decode(cidStr); err != nil {
		return nil, ErrBlobNotFound
	}

	// Operator-curated blocklist (M9): a direct indexed PK lookup, deny-first.
	if blocked, err := s.q.IsBlocklisted(ctx, cidStr); err != nil {
		return nil, fmt.Errorf("storage: blocklist check: %w", err)
	} else if blocked {
		return nil, ErrBlobBlocklisted
	}

	core, err := s.q.GetBlobCore(ctx, cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get blob core: %w", err)
	}

	switch core.State {
	case "active":
		// continue
	case "quarantined":
		return nil, ErrBlobQuarantined
	case "soft_deleted":
		return nil, ErrBlobSoftDeleted
	case "tombstoned":
		return nil, ErrBlobTombstoned
	default:
		return nil, fmt.Errorf("storage: unexpected blob state %q", core.State)
	}

	vis, err := s.q.ResolveEffectiveVisibility(ctx, cidStr)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve visibility: %w", err)
	}
	visibility := resolveVisibility(vis)
	// A private blob requires authorization. The signed-URL Guard grants it
	// per-request via WithReadAuthz after verifying a path-bound signature;
	// without a grant (and without a bearer path, which does not reach /blob),
	// the read is refused. M7.
	if visibility == VisibilityPrivate && !ReadAuthorized(ctx) {
		return nil, ErrBlobAuthRequired
	}

	size, err := s.q.GetManifestSize(ctx, cidStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("storage: missing manifest for %s", cidStr)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: get manifest size: %w", err)
	}

	view := &BlobView{
		CID:             core.Cid,
		MIME:            core.MimeType,
		PlaintextSize:   size,
		EnvelopeVersion: core.EnvelopeVersion,
		Product:         core.Product,
		UploadedAt:      core.UploadedAt,
		Visibility:      visibility,
		Encrypted:       core.Encrypted,
	}
	if core.OwnerID != "" {
		owner := core.OwnerID
		view.OwnerID = &owner
	}

	if core.Encrypted {
		dek, err := s.q.GetDEKByBlob(ctx, cidStr)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("storage: encrypted blob %s has no DEK row", cidStr)
		}
		if err != nil {
			return nil, fmt.Errorf("storage: get dek: %w", err)
		}
		if dek.State == "shredded" {
			return nil, ErrKeyShredded
		}
		mkvID, err := uuid.Parse(dek.MasterKeyVersionID)
		if err != nil {
			return nil, fmt.Errorf("storage: parse master key version id: %w", err)
		}
		view.wrappedKey = dek.WrappedKey
		view.masterKeyVersionID = &mkvID
	}

	return view, nil
}

// OpenBytes returns a reader over the blob's plaintext. For public_archival
// (unencrypted) blobs it streams directly from the backend (Range-friendly
// upstream). For encrypted blobs it fetches the whole envelope, unwraps the
// per-blob key, and decrypts in memory (v1 is single-shot; Phase 2 streaming
// AEAD removes the whole-object buffering). The caller MUST Close the reader.
func (s *Service) OpenBytes(ctx context.Context, v *BlobView) (io.ReadCloser, error) {
	c, err := cid.Decode(v.CID)
	if err != nil {
		return nil, fmt.Errorf("storage: decode cid: %w", err)
	}
	// P2-M4.1: guarantee the bytes are pinned locally before reading. On a local
	// hit this is a cheap Has + LRU touch; on a miss it either fetches+verifies
	// from a sourceable donor (and re-pins) or returns a sentinel
	// (ErrBlobNotFound when no donor tier is configured, ErrNoSourceableHolder
	// when configured but none can serve). The donor bytes are verified by
	// AddDeterministic BEFORE the decrypt path below runs — unverified ciphertext
	// is never unwrapped or served.
	if err := s.ensureLocal(ctx, c, v); err != nil {
		return nil, err
	}
	rc, err := s.backend.Get(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("storage: backend get: %w", err)
	}
	// P2-M4.1 transient mode: hold nothing beyond the in-flight read. The blob
	// was never marked present (admit is a no-op in transient), so we only need
	// to release the backend pin once the read is served, so the next committed
	// read is donor-backed again.
	transient := s.cache != nil && s.cache.mode() == StorageModeTransient
	if !v.Encrypted {
		// public_archival streams directly from the backend; defer the unpin to
		// the consumer's Close via the wrapper.
		if transient {
			return unpinOnClose{ReadCloser: rc, unpin: func() { s.cache.unpinBlob(context.WithoutCancel(ctx), c) }}, nil
		}
		return rc, nil
	}
	defer rc.Close()
	// Encrypted v1 buffers+decrypts fully before returning, so the backend pin is
	// no longer needed once the plaintext is in hand; unpin immediately.
	if transient {
		defer s.cache.unpinBlob(context.WithoutCancel(ctx), c)
	}

	env, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("storage: read envelope: %w", err)
	}
	if v.masterKeyVersionID == nil {
		return nil, fmt.Errorf("storage: encrypted view missing key material")
	}
	perBlobKey, err := s.ks.Unwrap(ctx, v.wrappedKey, *v.masterKeyVersionID)
	if err != nil {
		return nil, fmt.Errorf("storage: unwrap per-blob key: %w", err)
	}
	_, codec, err := envelope.Decode(env)
	if err != nil {
		return nil, fmt.Errorf("storage: decode envelope: %w", err)
	}
	plain, err := codec.Decrypt(env, perBlobKey)
	if err != nil {
		return nil, fmt.Errorf("storage: decrypt: %w", err)
	}
	return io.NopCloser(bytes.NewReader(plain)), nil
}
