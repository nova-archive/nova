package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

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
	maxUploadSize int64
	assembly      chan struct{} // buffered semaphore bounding in-memory assembly
	hook          WriteHook
}

// Option configures a Service. Read-only callers pass none.
type Option func(*svcOpts)

type svcOpts struct {
	maxUploadSize int64
	assemblySize  int
	hook          WriteHook
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

// NewService builds a storage service over the given pool, IPFS backend, and
// keystore. backend and ks may be nil in tests that exercise Resolve only.
// Write limits default to 100 MiB / 8 concurrent assemblies; override via
// WithWriteLimits. Existing read-only call sites pass no options.
func NewService(pool *pgxpool.Pool, backend ipfs.Backend, ks *envelope.Keystore, opts ...Option) *Service {
	o := svcOpts{maxUploadSize: 104857600, assemblySize: 8}
	for _, fn := range opts {
		fn(&o)
	}
	return &Service{
		q:             gen.New(pool),
		pool:          pool,
		backend:       backend,
		ks:            ks,
		maxUploadSize: o.maxUploadSize,
		assembly:      make(chan struct{}, o.assemblySize),
		hook:          o.hook,
	}
}

// Resolve loads and authorizes a blob for anonymous read. It performs no
// Kubo I/O and no decryption. Returns one of the package sentinels on a
// domain failure, or a wrapped error on infrastructure failure (→ 500).
func (s *Service) Resolve(ctx context.Context, cidStr string) (*BlobView, error) {
	if _, err := cid.Decode(cidStr); err != nil {
		return nil, ErrBlobNotFound
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

	vis, err := s.q.ResolveBlobVisibility(ctx, cidStr)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve visibility: %w", err)
	}
	visibility := resolveVisibility(vis)
	if visibility == VisibilityPrivate {
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
	rc, err := s.backend.Get(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("storage: backend get: %w", err)
	}
	if !v.Encrypted {
		return rc, nil
	}
	defer rc.Close()

	env, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("storage: read envelope: %w", err)
	}
	if v.masterKeyVersionID == nil {
		return nil, fmt.Errorf("storage: encrypted view missing key material")
	}
	perBlobKey, err := s.ks.Unwrap(v.wrappedKey, *v.masterKeyVersionID)
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
