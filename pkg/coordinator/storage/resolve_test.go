package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	mh "github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

// cidN returns a distinct, structurally-valid CIDv1 string for n.
func cidN(n int) string {
	h, err := mh.Sum([]byte{byte(n), 0xAB, 0xCD}, mh.SHA2_256, -1)
	if err != nil {
		panic(err)
	}
	return cid.NewCidV1(cid.Raw, h).String()
}

type seedOpts struct {
	cid        string
	state      string // blob_state
	visibility string // "" = no collection membership; else collection visibility
	encrypted  bool
	keyState   string // data_encryption_keys.state when encrypted
}

func seedBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, o seedOpts) {
	t.Helper()
	var ownerID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		o.cid+"@example.test").Scan(&ownerID))

	var keyID *uuid.UUID
	if o.encrypted {
		var mkv uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO master_key_versions (version_label, state) VALUES ($1,'active') RETURNING id`,
			"v1-"+o.cid).Scan(&mkv))
		var k uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
			 VALUES ('XChaCha20-Poly1305', $1, $2, $3) RETURNING id`,
			make([]byte, 72), mkv, o.keyState).Scan(&k))
		keyID = &k
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO blobs (cid, encryption_key_id, owner_id, mime_type, byte_size, state, product, envelope_version)
		 VALUES ($1,$2,$3,'application/octet-stream',10,$4,'raw',1)`,
		o.cid, keyID, ownerID, o.state)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256','raw','size-262144',10,58,1)`, o.cid)
	require.NoError(t, err)

	if o.visibility != "" {
		var col uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
			 VALUES ($1,$2,$2,$3,false) RETURNING id`,
			ownerID, "c-"+o.cid, o.visibility).Scan(&col))
		_, err = pool.Exec(ctx,
			`INSERT INTO collection_items (collection_id, blob_cid) VALUES ($1,$2)`, col, o.cid)
		require.NoError(t, err)
	}
}

func TestResolveMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool := dbtest.New(t, ctx)
	svc := storage.NewService(pool, nil, nil) // Resolve needs neither backend nor keystore

	cases := []struct {
		name    string
		o       seedOpts
		wantErr error
		wantVis storage.Visibility
		wantEnc bool
	}{
		{"public encrypted active", seedOpts{cidN(0), "active", "public", true, "active"}, nil, storage.VisibilityPublic, true},
		{"unlisted plaintext active", seedOpts{cidN(1), "active", "unlisted", false, ""}, nil, storage.VisibilityUnlisted, false},
		{"private active", seedOpts{cidN(2), "active", "private", true, "active"}, storage.ErrBlobAuthRequired, 0, false},
		{"no membership", seedOpts{cidN(3), "active", "", true, "active"}, storage.ErrBlobAuthRequired, 0, false},
		{"quarantined", seedOpts{cidN(4), "quarantined", "public", true, "active"}, storage.ErrBlobQuarantined, 0, false},
		{"soft_deleted", seedOpts{cidN(5), "soft_deleted", "public", true, "active"}, storage.ErrBlobSoftDeleted, 0, false},
		{"tombstoned", seedOpts{cidN(6), "tombstoned", "public", true, "shredded"}, storage.ErrBlobTombstoned, 0, false},
		{"key shredded but active", seedOpts{cidN(7), "active", "public", true, "shredded"}, storage.ErrKeyShredded, 0, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			seedBlob(t, ctx, pool, c.o)
			v, err := svc.Resolve(ctx, c.o.cid)
			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.wantVis, v.Visibility)
			require.Equal(t, c.wantEnc, v.Encrypted)
			require.Equal(t, int64(10), v.PlaintextSize)
		})
	}

	t.Run("not found", func(t *testing.T) {
		_, err := svc.Resolve(ctx, cidN(20))
		require.ErrorIs(t, err, storage.ErrBlobNotFound)
	})
	t.Run("bad cid", func(t *testing.T) {
		_, err := svc.Resolve(ctx, "not-a-cid")
		require.ErrorIs(t, err, storage.ErrBlobNotFound)
	})
}
