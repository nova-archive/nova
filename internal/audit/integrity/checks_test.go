package integrity_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	mh "github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/audit/integrity"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/stretchr/testify/require"
)

var errFakeNotFound = errors.New("fake backend: not found")

// fakeBackend is a stub backend: it returns exactly the bytes the test stored,
// letting checks exercise every pass/fail branch deterministically without
// booting Kubo.
type fakeBackend struct {
	objects map[string][]byte // cid → object bytes (Get)
	pinned  map[string]bool   // cid → Has
	blocks  map[string][]byte // block cid → raw bytes (BlockGet)
}

func newFake() *fakeBackend {
	return &fakeBackend{
		objects: map[string][]byte{},
		pinned:  map[string]bool{},
		blocks:  map[string][]byte{},
	}
}

func (f *fakeBackend) Get(_ context.Context, c cid.Cid) (io.ReadCloser, error) {
	b, ok := f.objects[c.String()]
	if !ok {
		return nil, errFakeNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeBackend) Has(_ context.Context, c cid.Cid) (bool, error) {
	return f.pinned[c.String()], nil
}

func (f *fakeBackend) BlockGet(_ context.Context, c cid.Cid) ([]byte, error) {
	b, ok := f.blocks[c.String()]
	if !ok {
		return nil, errFakeNotFound
	}
	return b, nil
}

func rawCID(t *testing.T, b []byte) cid.Cid {
	t.Helper()
	h, err := mh.Sum(b, mh.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, h)
}

func findByCID(findings []integrity.Finding, cidStr string) (integrity.Finding, bool) {
	for _, f := range findings {
		if f.CID == cidStr {
			return f, true
		}
	}
	return integrity.Finding{}, false
}

type checksFixture struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *gen.Queries
	ks     *envelope.Keystore
	fake   *fakeBackend
	checks map[integrity.Kind]integrity.Check
	mkvID  uuid.UUID
}

func setupChecks(t *testing.T) *checksFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t, ctx)

	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	mkvID, err := ks.Bootstrap(ctx)
	require.NoError(t, err)

	q := gen.New(pool)
	fake := newFake()
	return &checksFixture{
		ctx:    ctx,
		pool:   pool,
		q:      q,
		ks:     ks,
		fake:   fake,
		checks: integrity.NewChecks(q, fake, ks),
		mkvID:  mkvID,
	}
}

// run executes one kind's check with a large sample so every seeded row is
// returned, then locates the finding for the given CID.
func (fx *checksFixture) run(t *testing.T, kind integrity.Kind, cidStr string) integrity.Finding {
	t.Helper()
	findings, err := fx.checks[kind].Run(fx.ctx, 1000)
	require.NoError(t, err)
	f, ok := findByCID(findings, cidStr)
	require.True(t, ok, "no finding for %s in %s results", cidStr, kind)
	return f
}

// seedEncrypted inserts a real encrypted blob (DEK + blobs row) and serves its
// envelope through the fake backend. declaredSize overrides the blobs.byte_size
// (0 ⇒ len(plaintext)); tamper, if set, mutates the stored envelope bytes.
func (fx *checksFixture) seedEncrypted(t *testing.T, plaintext []byte, declaredSize int64, tamper func([]byte) []byte) (string, []byte) {
	t.Helper()
	pbk := make([]byte, envelope.KeySize)
	_, err := rand.Read(pbk)
	require.NoError(t, err)
	wrapped, mkvID, err := fx.ks.Wrap(pbk)
	require.NoError(t, err)
	env, err := envelope.V1().Encrypt(plaintext, pbk)
	require.NoError(t, err)

	stored := env
	if tamper != nil {
		stored = tamper(env)
	}
	cidStr := rawCID(t, env).String()
	fx.fake.objects[cidStr] = stored
	fx.fake.pinned[cidStr] = true

	size := declaredSize
	if size == 0 {
		size = int64(len(plaintext))
	}
	var keyID uuid.UUID
	require.NoError(t, fx.pool.QueryRow(fx.ctx,
		`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
		 VALUES ('XChaCha20-Poly1305',$1,$2,'active') RETURNING id`, wrapped, mkvID).Scan(&keyID))
	_, err = fx.pool.Exec(fx.ctx,
		`INSERT INTO blobs (cid, encryption_key_id, mime_type, byte_size, state, product)
		 VALUES ($1,$2,'application/octet-stream',$3,'active','raw')`, cidStr, keyID, size)
	require.NoError(t, err)
	return cidStr, env
}

// seedEncryptedWithBadDEK inserts a blob whose wrapped_key is garbage (valid
// master version) so key_unwrap fails.
func (fx *checksFixture) seedEncryptedWithBadDEK(t *testing.T) string {
	t.Helper()
	garbage := make([]byte, envelope.WrappedKeySize)
	_, err := rand.Read(garbage)
	require.NoError(t, err)
	cidStr := rawCID(t, append([]byte("baddek"), garbage[:8]...)).String()
	fx.fake.pinned[cidStr] = true

	var keyID uuid.UUID
	require.NoError(t, fx.pool.QueryRow(fx.ctx,
		`INSERT INTO data_encryption_keys (algorithm, wrapped_key, master_key_version_id, state)
		 VALUES ('XChaCha20-Poly1305',$1,$2,'active') RETURNING id`, garbage, fx.mkvID).Scan(&keyID))
	_, err = fx.pool.Exec(fx.ctx,
		`INSERT INTO blobs (cid, encryption_key_id, mime_type, byte_size, state, product)
		 VALUES ($1,$2,'application/octet-stream',10,'active','raw')`, cidStr, keyID)
	require.NoError(t, err)
	return cidStr
}

func (fx *checksFixture) insertBareBlob(t *testing.T, cidStr, state string) {
	t.Helper()
	_, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO blobs (cid, mime_type, byte_size, state, product)
		 VALUES ($1,'application/octet-stream',10,$2,'raw')`, cidStr, state)
	require.NoError(t, err)
}

func (fx *checksFixture) insertDerivative(t *testing.T, parentCID, derivCID, state string) {
	t.Helper()
	_, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO blobs (cid, parent_cid, derivative_preset, derivative_format, mime_type, byte_size, state, product)
		 VALUES ($1,$2,'thumb','webp','image/webp',10,$3,'raw')`, derivCID, parentCID, state)
	require.NoError(t, err)
}

func (fx *checksFixture) insertManifest(t *testing.T, cidStr string, blockCount int32, envelopeSize int64) {
	t.Helper()
	_, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256','dag-pb','size-262144',$2,$2,$3)`, cidStr, envelopeSize, blockCount)
	require.NoError(t, err)
}

func (fx *checksFixture) insertBlock(t *testing.T, blobCID, blockCID string, idx, size int) {
	t.Helper()
	_, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO blob_blocks (blob_cid, block_cid, block_index, block_size)
		 VALUES ($1,$2,$3,$4)`, blobCID, blockCID, idx, size)
	require.NoError(t, err)
}

func TestIntegrityChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integrity checks DB test in short mode")
	}
	fx := setupChecks(t)

	t.Run("clean encrypted blob passes envelope_decode, key_unwrap, sample_decrypt", func(t *testing.T) {
		cidStr, _ := fx.seedEncrypted(t, []byte("clean payload"), 0, nil)
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindEnvelopeDecode, cidStr).Result)
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindKeyUnwrap, cidStr).Result)
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindSampleDecrypt, cidStr).Result)
	})

	t.Run("envelope_decode fails on a corrupted header", func(t *testing.T) {
		cidStr, _ := fx.seedEncrypted(t, []byte("payload"), 0, func(env []byte) []byte {
			bad := append([]byte(nil), env...)
			bad[0] ^= 0xFF // break the magic
			return bad
		})
		require.Equal(t, integrity.ResultFail, fx.run(t, integrity.KindEnvelopeDecode, cidStr).Result)
	})

	t.Run("sample_decrypt fails on tampered ciphertext", func(t *testing.T) {
		cidStr, _ := fx.seedEncrypted(t, []byte("payload to tamper"), 0, func(env []byte) []byte {
			bad := append([]byte(nil), env...)
			bad[len(bad)-1] ^= 0xFF // flip a tag byte; header intact
			return bad
		})
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindEnvelopeDecode, cidStr).Result)
		require.Equal(t, integrity.ResultFail, fx.run(t, integrity.KindSampleDecrypt, cidStr).Result)
	})

	t.Run("sample_decrypt skips oversize blobs", func(t *testing.T) {
		cidStr, _ := fx.seedEncrypted(t, []byte("small bytes"), 9<<20, nil)
		require.Equal(t, integrity.ResultSkip, fx.run(t, integrity.KindSampleDecrypt, cidStr).Result)
	})

	t.Run("key_unwrap fails on a corrupt wrapped key", func(t *testing.T) {
		cidStr := fx.seedEncryptedWithBadDEK(t)
		require.Equal(t, integrity.ResultFail, fx.run(t, integrity.KindKeyUnwrap, cidStr).Result)
	})

	t.Run("kubo_pin_present pass and fail", func(t *testing.T) {
		good := rawCID(t, []byte("pinned-obj")).String()
		fx.insertBareBlob(t, good, "active")
		fx.fake.pinned[good] = true
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindKuboPinPresent, good).Result)

		bad := rawCID(t, []byte("unpinned-obj")).String()
		fx.insertBareBlob(t, bad, "active")
		f := fx.run(t, integrity.KindKuboPinPresent, bad)
		require.Equal(t, integrity.ResultFail, f.Result)
		require.Contains(t, f.Detail, "not pinned")
	})

	t.Run("block_hash_valid passes intact blocks, fails corrupted blocks", func(t *testing.T) {
		b1 := []byte("first-block-bytes")
		c1 := rawCID(t, b1)
		c2 := rawCID(t, []byte("second-block-bytes"))
		blobCID := rawCID(t, []byte("multiblock-root")).String()
		fx.insertBareBlob(t, blobCID, "active")
		fx.insertManifest(t, blobCID, 2, 999)
		fx.insertBlock(t, blobCID, c1.String(), 0, len(b1))
		fx.insertBlock(t, blobCID, c2.String(), 1, 18)
		fx.fake.blocks[c1.String()] = b1                  // intact
		fx.fake.blocks[c2.String()] = []byte("CORRUPTED") // wrong bytes ⇒ rehash mismatch

		findings, err := fx.checks[integrity.KindBlockHashValid].Run(fx.ctx, 1000)
		require.NoError(t, err)
		var sawPass, sawFail bool
		for _, f := range findings {
			if f.CID != blobCID {
				continue
			}
			switch f.Result {
			case integrity.ResultPass:
				sawPass = true
			case integrity.ResultFail:
				sawFail = true
			}
		}
		require.True(t, sawPass, "intact block should pass")
		require.True(t, sawFail, "corrupted block should fail")
	})

	t.Run("manifest_consistent pass and fail", func(t *testing.T) {
		ok := rawCID(t, []byte("manifest-ok")).String()
		fx.insertBareBlob(t, ok, "active")
		fx.insertManifest(t, ok, 1, 20)
		fx.insertBlock(t, ok, rawCID(t, []byte("ok-blk")).String(), 0, 20)
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindManifestConsistent, ok).Result)

		bad := rawCID(t, []byte("manifest-bad")).String()
		fx.insertBareBlob(t, bad, "active")
		fx.insertManifest(t, bad, 1, 100)                                    // claims 100 envelope bytes...
		fx.insertBlock(t, bad, rawCID(t, []byte("bad-blk")).String(), 0, 10) // ...but the one block is 10
		require.Equal(t, integrity.ResultFail, fx.run(t, integrity.KindManifestConsistent, bad).Result)
	})

	t.Run("derivative_state_consistent pass and fail", func(t *testing.T) {
		p1 := rawCID(t, []byte("parent-active")).String()
		d1 := rawCID(t, []byte("deriv-active")).String()
		fx.insertBareBlob(t, p1, "active")
		fx.insertDerivative(t, p1, d1, "active")
		require.Equal(t, integrity.ResultPass, fx.run(t, integrity.KindDerivativeStateConsistent, d1).Result)

		p2 := rawCID(t, []byte("parent-quarantined")).String()
		d2 := rawCID(t, []byte("deriv-orphan")).String()
		fx.insertBareBlob(t, p2, "quarantined")
		fx.insertDerivative(t, p2, d2, "active")
		require.Equal(t, integrity.ResultFail, fx.run(t, integrity.KindDerivativeStateConsistent, d2).Result)
	})
}
