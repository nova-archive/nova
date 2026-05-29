package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

func TestValidateMIME(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	webp := append(append([]byte("RIFF"), 0, 0, 0, 0), []byte("WEBPVP8 ")...)
	script := []byte("#!/bin/sh\necho hi\n")

	cases := []struct {
		name     string
		declared string
		body     []byte
		want     string
		wantErr  bool
	}{
		{"jpeg ok", "image/jpeg", jpeg, "image/jpeg", false},
		{"png ok", "image/png", png, "image/png", false},
		{"webp ok", "image/webp", webp, "image/webp", false},
		{"empty declared uses detected", "", png, "image/png", false},
		{"unknown sniff accepts declared", "image/avif", []byte{0, 0, 0, 0x1c}, "image/avif", false},
		{"contradiction rejected", "image/jpeg", script, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateMIME(tc.declared, tc.body)
			if tc.wantErr {
				require.ErrorIs(t, err, ErrMimeRejected)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// fakeBackend implements ipfs.Backend in memory, deriving a deterministic
// CIDv1/raw from the bytes and recording Unpin calls.
type fakeBackend struct {
	store    map[string][]byte
	unpinned []string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{store: map[string][]byte{}} }

func (f *fakeBackend) AddDeterministic(ctx context.Context, env []byte) (ipfs.AddResult, error) {
	c, err := cid.V1Builder{Codec: cid.Raw, MhType: multihash.SHA2_256}.Sum(env)
	if err != nil {
		return ipfs.AddResult{}, err
	}
	f.store[c.String()] = append([]byte(nil), env...)
	return ipfs.AddResult{
		CID: c, EnvelopeSize: int64(len(env)), Codec: "raw",
		Blocks: []ipfs.Block{{CID: c, Index: 0, Size: len(env)}}, MerkleRoot: c,
	}, nil
}

func (f *fakeBackend) Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error) {
	b, ok := f.store[c.String()]
	if !ok {
		return nil, fmt.Errorf("fakeBackend: %s not found", c)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (f *fakeBackend) Has(ctx context.Context, c cid.Cid) (bool, error) {
	_, ok := f.store[c.String()]
	return ok, nil
}
func (f *fakeBackend) Pin(ctx context.Context, c cid.Cid) error { return nil }
func (f *fakeBackend) Unpin(ctx context.Context, c cid.Cid) error {
	f.unpinned = append(f.unpinned, c.String())
	return nil
}
func (f *fakeBackend) BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error) {
	_, ok := f.store[c.String()]
	return ok, nil
}
func (f *fakeBackend) BlockGet(ctx context.Context, c cid.Cid) ([]byte, error) {
	return f.store[c.String()], nil
}
func (f *fakeBackend) Close(ctx context.Context) error { return nil }

func bootstrapKS(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *envelope.Keystore {
	t.Helper()
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	return ks
}

func seedCollection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, visibility string, publicArchival bool) uuid.UUID {
	t.Helper()
	var owner, col uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, role) VALUES ($1,'operator') RETURNING id`,
		uuid.NewString()+"@put.test").Scan(&owner))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO collections (owner_id, name, slug, visibility, public_archival)
		 VALUES ($1,'c','c',$2,$3) RETURNING id`, owner, visibility, publicArchival).Scan(&col))
	return col
}

func TestIntegrationPutEncryptedRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", false) // encrypted blob in a public collection

	body := []byte("hello nova upload")
	res, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col})
	require.NoError(t, err)
	require.True(t, res.Encrypted)
	require.Equal(t, int64(len(body)), res.ByteSize)
	require.NotEqual(t, body, fb.store[res.CID], "stored bytes must be an envelope, not plaintext")

	view, err := svc.Resolve(ctx, res.CID)
	require.NoError(t, err)
	rc, err := svc.OpenBytes(ctx, view)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, body, got)
}

func TestIntegrationPutPublicArchivalPlaintext(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	col := seedCollection(t, ctx, pool, "public", true)

	body := []byte("public archival data")
	res, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col})
	require.NoError(t, err)
	require.False(t, res.Encrypted)
	require.Equal(t, body, fb.store[res.CID], "public_archival stores plaintext, no envelope")
}

func TestIntegrationPutRollbackUnpinsOnCommitFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	fb := newFakeBackend()
	svc := NewService(pool, fb, ks)
	body := []byte("will roll back")

	// Unknown collection is resolved before import → no pin, no unpin.
	bogus := uuid.New()
	_, err := svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &bogus})
	require.ErrorIs(t, err, ErrCollectionNotFound)
	require.Empty(t, fb.unpinned)

	// A nonexistent owner makes InsertBlob's FK fail at commit time → the
	// orphaned import must be unpinned.
	col := seedCollection(t, ctx, pool, "private", false)
	ghost := uuid.New()
	_, err = svc.Put(ctx, bytes.NewReader(body), int64(len(body)),
		PutContext{MIME: "text/plain", Product: "raw", CollectionID: &col, OwnerID: &ghost})
	require.Error(t, err)
	require.Len(t, fb.unpinned, 1)
}

func TestIntegrationPutTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	ks := bootstrapKS(t, ctx, pool)
	svc := NewService(pool, newFakeBackend(), ks, WithWriteLimits(8, 4))
	_, err := svc.Put(ctx, bytes.NewReader([]byte("123456789")), 9,
		PutContext{MIME: "text/plain", Product: "raw"})
	require.ErrorIs(t, err, ErrUploadTooLarge)
}
