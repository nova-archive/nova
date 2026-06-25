package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	if max := s.maxUploadSize.Load(); max > 0 && declaredSize > max {
		return nil, ErrUploadTooLarge
	}
	release, ok := s.tryAcquireAssembly()
	if !ok {
		return nil, ErrServerBusy
	}
	defer release()

	buf := make([]byte, declaredSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("storage: read upload body: %w", err)
	}

	mime, err := validateMIME(pc.MIME, buf)
	if err != nil {
		return nil, err
	}

	var persist func(context.Context, pgx.Tx, string) error
	if s.hook != nil {
		ar, herr := s.hook.Analyze(ctx, pc, buf)
		if herr != nil {
			return nil, herr
		}
		if ar.Scan.Action != ActionAllow {
			return nil, ErrModerationRejected
		}
		if ar.Transformed != nil {
			buf = ar.Transformed
			if ar.ResultMIME != "" {
				mime = ar.ResultMIME
			}
			if max := s.maxUploadSize.Load(); max > 0 && int64(len(buf)) > max {
				return nil, ErrUploadTooLarge
			}
		}
		persist = ar.Persist
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

	// Refuse a blocklisted CID before commit; unpin the just-added orphan. The
	// CID is only known post-import (encrypted envelopes are nonce-randomised),
	// so this is pin-then-unpin for the rare blocklisted upload. Effective for
	// public_archival re-uploads (deterministic CID). M9.
	if blocked, berr := s.q.IsBlocklisted(ctx, add.CID.String()); berr != nil {
		_ = s.backend.Unpin(ctx, add.CID)
		return nil, fmt.Errorf("storage: blocklist check: %w", berr)
	} else if blocked {
		_ = s.backend.Unpin(ctx, add.CID)
		return nil, ErrBlobBlocklisted
	}

	if err := s.commit(ctx, add, buf, mime, pc, encrypt, wrapped, mkvID, persist); err != nil {
		if uerr := s.backend.Unpin(ctx, add.CID); uerr != nil {
			err = fmt.Errorf("%w (unpin also failed: %v)", err, uerr)
		}
		return nil, fmt.Errorf("storage: commit: %w", err)
	}

	if s.hook != nil {
		s.hook.OnCommitted(ctx, CommittedRef{CID: add.CID.String(), Product: pc.Product})
	}

	// Gate-off best-effort admission assign (P2-M4.1, Task 10). The blob is
	// already durable on the coordinator; a failed assign MUST NOT fail the
	// upload. The gate-on path (Task 11: require_replication_quorum_before_commit)
	// will call Assign before returning — that logic is not implemented here yet.
	if s.assigner != nil {
		cidStr := add.CID.String()
		if _, aerr := s.assigner.Assign(ctx, cidStr, classFor(pc)); aerr != nil {
			slog.Warn("admission.assign_failed", "cid", cidStr, "err", aerr)
		}
	}

	return &PutResult{
		CID: add.CID.String(), ByteSize: int64(len(buf)),
		MIME: mime, Product: pc.Product, Encrypted: encrypt,
	}, nil
}

// classFor returns the durability class string for a given PutContext.
// All user-uploaded originals are "important" (high durability default);
// refinement by product type is a later-task concern.
func classFor(_ PutContext) string { return "important" }

func (s *Service) commit(ctx context.Context, add ipfs.AddResult, buf []byte, mime string, pc PutContext, encrypt bool, wrapped []byte, mkvID uuid.UUID, persist func(context.Context, pgx.Tx, string) error) error {
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
	if persist != nil {
		if err := persist(ctx, tx, cidStr); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// pgUUID wraps a google/uuid value as a non-null pgtype.UUID.
func pgUUID(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
