// Package blobfixture seeds a fully-formed blob (encrypt → import to Kubo →
// DB rows) for read-path tests. It stands in for the M4 write pipeline.
// Not for production use; it performs raw INSERTs and trusts its inputs.
package blobfixture

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
)

// Deps are the live subsystems the fixture writes through.
type Deps struct {
	Pool     *pgxpool.Pool
	Backend  ipfs.Backend
	Keystore *envelope.Keystore
}

// Spec describes the blob to create.
type Spec struct {
	Plaintext   []byte
	MIME        string
	Visibility  string // "public" | "unlisted" | "private" | "" (no membership)
	State       string // blob_state; defaults to "active"
	Unencrypted bool   // Unencrypted seeds a public_archival blob: raw plaintext imported to Kubo,
	// encryption_key_id NULL, forced into a public collection. Keystore is not used.
}

// Result reports what was created.
type Result struct {
	CID        string
	ParsedCID  cid.Cid
	PerBlobKey []byte
	OwnerID    uuid.UUID
}

// Seed encrypts the plaintext, imports the envelope to Kubo, and inserts
// users/data_encryption_keys/blobs/blob_manifests/blob_blocks/collections/
// collection_items rows so the read path can serve it.
func Seed(ctx context.Context, d Deps, s Spec) (Result, error) {
	if s.State == "" {
		s.State = "active"
	}

	var (
		stored  []byte
		pbk     []byte
		wrapped []byte
		mkvID   uuid.UUID
	)
	if s.Unencrypted {
		stored = s.Plaintext
	} else {
		pbk = make([]byte, envelope.KeySize)
		if _, err := rand.Read(pbk); err != nil {
			return Result{}, fmt.Errorf("blobfixture: rand key: %w", err)
		}
		var err error
		wrapped, mkvID, err = d.Keystore.Wrap(pbk)
		if err != nil {
			return Result{}, fmt.Errorf("blobfixture: wrap: %w", err)
		}
		env, err := envelope.V1().Encrypt(s.Plaintext, pbk)
		if err != nil {
			return Result{}, fmt.Errorf("blobfixture: encrypt: %w", err)
		}
		stored = env
	}

	add, err := d.Backend.AddDeterministic(ctx, stored)
	if err != nil {
		return Result{}, fmt.Errorf("blobfixture: import: %w", err)
	}
	cidStr := add.CID.String()

	var ownerID uuid.UUID
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		cidStr+"@fixture.test").Scan(&ownerID); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert user: %w", err)
	}

	var keyID *uuid.UUID
	if !s.Unencrypted {
		var k uuid.UUID
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
			 VALUES ('XChaCha20-Poly1305', $1, $2, 'active') RETURNING id`,
			wrapped, mkvID).Scan(&k); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert dek: %w", err)
		}
		keyID = &k
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, state, product, envelope_version)
		 VALUES ($1,$2,$3,$4,$5,$6,'raw',1)`,
		cidStr, keyID, ownerID, s.MIME, len(s.Plaintext), s.State); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert blob: %w", err)
	}

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256',$2,'size-262144',$3,$4,$5)`,
		cidStr, add.Codec, len(s.Plaintext), add.EnvelopeSize, len(add.Blocks)); err != nil {
		return Result{}, fmt.Errorf("blobfixture: insert manifest: %w", err)
	}
	for _, b := range add.Blocks {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
			 VALUES ($1,$2,$3,$4)`, cidStr, b.CID.String(), b.Index, b.Size); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert block: %w", err)
		}
	}

	if s.Visibility != "" || s.Unencrypted {
		visibility := s.Visibility
		publicArchival := false
		if s.Unencrypted {
			// public_archival blobs must live in a public collection (DB CHECK
			// public_archival_requires_public_visibility).
			visibility = "public"
			publicArchival = true
		}
		var col uuid.UUID
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
			 VALUES ($1,$2,$2,$3,$4) RETURNING id`,
			ownerID, "col-"+cidStr[:12], visibility, publicArchival).Scan(&col); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert collection: %w", err)
		}
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`,
			col, cidStr); err != nil {
			return Result{}, fmt.Errorf("blobfixture: insert collection_item: %w", err)
		}
	}

	return Result{CID: cidStr, ParsedCID: add.CID, PerBlobKey: pbk, OwnerID: ownerID}, nil
}
