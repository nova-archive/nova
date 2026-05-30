package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// DerivativeContext carries validated derivative-write metadata.
type DerivativeContext struct {
	ParentCID string // REFERENCES blobs(cid); authorizes + cascades
	Preset    string // canonical key part: 'thumb' | 'w512' | '512x384'
	Format    string // 'webp' | 'jpeg' | 'png' | 'avif' | 'jxl'
	MIME      string
	Width     int
	Height    int
}

// PutDerivative encrypts a derivative under a fresh per-blob key, imports it
// deterministically, and commits blobs(+derivative cols) + manifest + blocks +
// DEK in one transaction; persist (non-nil) writes the product side-table row
// in the same tx. Idempotent under the (parent,preset,format) unique index: a
// loser unpins its orphan import and returns the winner's CID. The assembly
// semaphore bounds the in-memory encrypt+import window, same as Put.
func (s *Service) PutDerivative(ctx context.Context, plaintext []byte, dc DerivativeContext,
	persist func(ctx context.Context, tx pgx.Tx, cid string) error) (*PutResult, error) {

	select {
	case s.assembly <- struct{}{}:
		defer func() { <-s.assembly }()
	default:
		return nil, ErrServerBusy
	}

	pbk := make([]byte, envelope.KeySize)
	if _, err := rand.Read(pbk); err != nil {
		return nil, fmt.Errorf("storage: derivative key: %w", err)
	}
	wrapped, mkvID, err := s.ks.Wrap(pbk)
	if err != nil {
		return nil, fmt.Errorf("storage: wrap: %w", err)
	}
	env, err := envelope.V1().Encrypt(plaintext, pbk)
	if err != nil {
		return nil, fmt.Errorf("storage: encrypt: %w", err)
	}
	add, err := s.backend.AddDeterministic(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("storage: import: %w", err)
	}

	won, err := s.commitDerivative(ctx, add, plaintext, dc, wrapped, mkvID, persist)
	if err != nil {
		if uerr := s.backend.Unpin(ctx, add.CID); uerr != nil {
			err = fmt.Errorf("%w (unpin failed: %v)", err, uerr)
		}
		return nil, fmt.Errorf("storage: derivative commit: %w", err)
	}
	if !won {
		_ = s.backend.Unpin(ctx, add.CID) // lost the race: drop our orphan import
		winner, gerr := s.q.GetDerivativeCID(ctx, gen.GetDerivativeCIDParams{
			ParentCid:        pgtype.Text{String: dc.ParentCID, Valid: true},
			DerivativePreset: pgtype.Text{String: dc.Preset, Valid: true},
			DerivativeFormat: pgtype.Text{String: dc.Format, Valid: true},
		})
		if gerr != nil {
			return nil, fmt.Errorf("storage: lookup winner: %w", gerr)
		}
		return &PutResult{CID: winner, ByteSize: int64(len(plaintext)), MIME: dc.MIME, Product: "image", Encrypted: true}, nil
	}
	return &PutResult{CID: add.CID.String(), ByteSize: int64(len(plaintext)), MIME: dc.MIME, Product: "image", Encrypted: true}, nil
}

// commitDerivative returns won=false when the (parent,preset,format) unique
// index rejected the insert (0 rows) — the caller recovers gracefully.
func (s *Service) commitDerivative(ctx context.Context, add ipfs.AddResult, plaintext []byte, dc DerivativeContext,
	wrapped []byte, mkvID uuid.UUID, persist func(context.Context, pgx.Tx, string) error) (bool, error) {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	cidStr := add.CID.String()

	keyID, err := qtx.InsertDEK(ctx, gen.InsertDEKParams{
		Algorithm: "XChaCha20-Poly1305", WrappedKey: wrapped, MasterKeyVersionID: pgUUID(mkvID),
	})
	if err != nil {
		return false, err
	}
	rows, err := qtx.InsertDerivativeBlob(ctx, gen.InsertDerivativeBlobParams{
		Cid:              cidStr,
		EncryptionKeyID:  keyID,
		ParentCid:        pgtype.Text{String: dc.ParentCID, Valid: true},
		DerivativePreset: pgtype.Text{String: dc.Preset, Valid: true},
		DerivativeFormat: pgtype.Text{String: dc.Format, Valid: true},
		MimeType:         dc.MIME,
		ByteSize:         int64(len(plaintext)),
	})
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil // unique-index conflict: roll back (defer) + signal loser path
	}
	var mr pgtype.Text
	if len(add.Blocks) > 1 {
		mr = pgtype.Text{String: add.MerkleRoot.String(), Valid: true}
	}
	if err := qtx.InsertManifest(ctx, gen.InsertManifestParams{
		Cid: cidStr, HashAlg: "sha2-256", Codec: add.Codec, Chunker: "size-262144",
		PlaintextSize: int64(len(plaintext)), EnvelopeSize: add.EnvelopeSize,
		BlockCount: int32(len(add.Blocks)), MerkleRoot: mr,
	}); err != nil {
		return false, err
	}
	for _, b := range add.Blocks {
		if err := qtx.InsertBlock(ctx, gen.InsertBlockParams{
			BlobCid: cidStr, BlockCid: b.CID.String(), BlockIndex: int32(b.Index), BlockSize: int32(b.Size),
		}); err != nil {
			return false, err
		}
	}
	if persist != nil {
		if err := persist(ctx, tx, cidStr); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// GetDerivativeCID returns the CID of the cached derivative for
// (parent, preset, format), and whether one exists. No row ⇒ ("", false, nil).
func (s *Service) GetDerivativeCID(ctx context.Context, parent, preset, format string) (string, bool, error) {
	cid, err := s.q.GetDerivativeCID(ctx, gen.GetDerivativeCIDParams{
		ParentCid:        pgtype.Text{String: parent, Valid: true},
		DerivativePreset: pgtype.Text{String: preset, Valid: true},
		DerivativeFormat: pgtype.Text{String: format, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return cid, true, nil
}
