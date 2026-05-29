package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// validateMIME is a cheap generic content floor. It blocks the XSS-relevant
// case of a text/script body declared as an image, without rejecting formats
// the stdlib sniffer doesn't recognize (e.g. AVIF → octet-stream). It does NOT
// prove the bytes are a valid instance of the declared type — that is the
// product layer's (M5) decode validation.
//
// Rules: empty declared ⇒ use detected; detected octet-stream (unknown) ⇒
// trust the declaration; otherwise reject when the detected top-level type
// contradicts the declared one.
func validateMIME(declared string, head []byte) (string, error) {
	detected := http.DetectContentType(head) // reads up to the first 512 bytes
	if declared == "" {
		return detected, nil
	}
	if detected == "application/octet-stream" {
		return declared, nil
	}
	if topLevel(detected) != topLevel(declared) {
		return "", fmt.Errorf("%w: declared %q, detected %q", ErrMimeRejected, declared, detected)
	}
	return declared, nil
}

func topLevel(mime string) string {
	if i := strings.IndexByte(mime, '/'); i >= 0 {
		return mime[:i]
	}
	return mime
}

// Put encrypts (or, for public_archival collections, stores plaintext),
// imports deterministically to Kubo, and commits the blob + manifest + blocks
// (+ DEK, + collection membership) in one transaction. On any commit failure
// it best-effort unpins the orphaned Kubo import.
//
// The Kubo pin precedes the DB commit and the two cannot be made atomic: a hard
// crash in between leaks a pinned, unreadable CID (documented residual risk; a
// reconciliation sweep is out of M4 scope). Put owns the in-memory window — a
// buffered-channel semaphore bounds concurrent assemblies — and reads exactly
// declaredSize bytes from r.
func (s *Service) Put(ctx context.Context, r io.Reader, declaredSize int64, pc PutContext) (*PutResult, error) {
	if declaredSize > s.maxUploadSize {
		return nil, ErrUploadTooLarge
	}
	select {
	case s.assembly <- struct{}{}:
		defer func() { <-s.assembly }()
	default:
		return nil, ErrServerBusy
	}

	buf := make([]byte, declaredSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("storage: read upload body: %w", err)
	}

	mime, err := validateMIME(pc.MIME, buf)
	if err != nil {
		return nil, err
	}

	encrypt := true
	if pc.CollectionID != nil {
		col, err := s.q.GetCollectionForWrite(ctx, pgUUID(*pc.CollectionID))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCollectionNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("storage: get collection: %w", err)
		}
		if col.PublicArchival {
			encrypt = false
		}
	}

	var (
		stored  []byte
		wrapped []byte
		mkvID   uuid.UUID
	)
	if encrypt {
		pbk := make([]byte, envelope.KeySize)
		if _, err := rand.Read(pbk); err != nil {
			return nil, fmt.Errorf("storage: generate per-blob key: %w", err)
		}
		w, id, err := s.ks.Wrap(pbk)
		if err != nil {
			return nil, fmt.Errorf("storage: wrap key: %w", err)
		}
		env, err := envelope.V1().Encrypt(buf, pbk)
		if err != nil {
			return nil, fmt.Errorf("storage: encrypt: %w", err)
		}
		wrapped, mkvID, stored = w, id, env
	} else {
		stored = buf
	}

	add, err := s.backend.AddDeterministic(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("storage: import: %w", err)
	}

	if err := s.commit(ctx, add, buf, mime, pc, encrypt, wrapped, mkvID); err != nil {
		if uerr := s.backend.Unpin(ctx, add.CID); uerr != nil {
			err = fmt.Errorf("%w (unpin also failed: %v)", err, uerr)
		}
		return nil, fmt.Errorf("storage: commit: %w", err)
	}

	return &PutResult{
		CID: add.CID.String(), ByteSize: int64(len(buf)),
		MIME: mime, Product: pc.Product, Encrypted: encrypt,
	}, nil
}

func (s *Service) commit(ctx context.Context, add ipfs.AddResult, buf []byte, mime string, pc PutContext, encrypt bool, wrapped []byte, mkvID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)
	cidStr := add.CID.String()

	var keyID pgtype.UUID // zero value ⟹ NULL (public_archival plaintext)
	if encrypt {
		id, err := qtx.InsertDEK(ctx, gen.InsertDEKParams{
			Algorithm: "XChaCha20-Poly1305", WrappedKey: wrapped, MasterKeyVersionID: pgUUID(mkvID),
		})
		if err != nil {
			return err
		}
		keyID = id
	}

	var owner pgtype.UUID
	if pc.OwnerID != nil {
		owner = pgUUID(*pc.OwnerID)
	}
	var srcIP *netip.Addr
	if pc.SourceIP.IsValid() {
		ip := pc.SourceIP
		srcIP = &ip
	}
	if err := qtx.InsertBlob(ctx, gen.InsertBlobParams{
		Cid: cidStr, EncryptionKeyID: keyID, OwnerID: owner,
		MimeType: mime, ByteSize: int64(len(buf)), SourceIp: srcIP,
		Product: gen.BlobProduct(pc.Product),
	}); err != nil {
		return err
	}

	var mr pgtype.Text
	if len(add.Blocks) > 1 {
		mr = pgtype.Text{String: add.MerkleRoot.String(), Valid: true}
	}
	if err := qtx.InsertManifest(ctx, gen.InsertManifestParams{
		Cid: cidStr, HashAlg: "sha2-256", Codec: add.Codec, Chunker: "size-262144",
		PlaintextSize: int64(len(buf)), EnvelopeSize: add.EnvelopeSize,
		BlockCount: int32(len(add.Blocks)), MerkleRoot: mr,
	}); err != nil {
		return err
	}
	for _, b := range add.Blocks {
		if err := qtx.InsertBlock(ctx, gen.InsertBlockParams{
			BlobCid: cidStr, BlockCid: b.CID.String(),
			BlockIndex: int32(b.Index), BlockSize: int32(b.Size),
		}); err != nil {
			return err
		}
	}
	if pc.CollectionID != nil {
		if err := qtx.InsertCollectionItem(ctx, gen.InsertCollectionItemParams{
			CollectionID: pgUUID(*pc.CollectionID), BlobCid: cidStr,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// pgUUID wraps a google/uuid value as a non-null pgtype.UUID.
func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
