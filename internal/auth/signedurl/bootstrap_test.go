package signedurl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/stretchr/testify/require"
)

type fakeBootQ struct {
	count    int64
	inserted []gen.InsertSigningKeyParams
}

func (f *fakeBootQ) CountActiveSigningKeys(_ context.Context) (int64, error) { return f.count, nil }

func (f *fakeBootQ) InsertSigningKey(_ context.Context, arg gen.InsertSigningKeyParams) error {
	f.inserted = append(f.inserted, arg)
	f.count++
	return nil
}

type fakeWrap struct {
	mvid          uuid.UUID
	lastSecretLen int
}

func (f *fakeWrap) Wrap(secret []byte) ([]byte, uuid.UUID, error) {
	f.lastSecretLen = len(secret)
	return append([]byte("wrapped:"), secret...), f.mvid, nil
}

func TestEnsureActiveKeyMintsWhenEmpty(t *testing.T) {
	t.Parallel()
	q := &fakeBootQ{count: 0}
	w := &fakeWrap{mvid: uuid.New()}
	require.NoError(t, signedurl.EnsureActiveKey(context.Background(), q, w))

	require.Len(t, q.inserted, 1)
	ins := q.inserted[0]
	require.True(t, strings.HasPrefix(ins.Kid, "k_"), "kid=%q", ins.Kid)
	require.NotEmpty(t, ins.WrappedKey)
	require.True(t, ins.MasterKeyVersionID.Valid)
	require.Equal(t, 32, w.lastSecretLen, "256-bit secret")
}

func TestEnsureActiveKeyIdempotent(t *testing.T) {
	t.Parallel()
	q := &fakeBootQ{count: 1} // an active key already exists
	w := &fakeWrap{mvid: uuid.New()}
	require.NoError(t, signedurl.EnsureActiveKey(context.Background(), q, w))
	require.Empty(t, q.inserted, "no mint when an active key is present")

	// A second call after a real first mint is also a no-op.
	q2 := &fakeBootQ{count: 0}
	require.NoError(t, signedurl.EnsureActiveKey(context.Background(), q2, w))
	require.NoError(t, signedurl.EnsureActiveKey(context.Background(), q2, w))
	require.Len(t, q2.inserted, 1)
}

func TestMintKeyUniqueKIDs(t *testing.T) {
	t.Parallel()
	q := &fakeBootQ{}
	w := &fakeWrap{mvid: uuid.New()}
	k1, err := signedurl.MintKey(context.Background(), q, w)
	require.NoError(t, err)
	k2, err := signedurl.MintKey(context.Background(), q, w)
	require.NoError(t, err)
	require.NotEqual(t, k1, k2)
	require.True(t, strings.HasPrefix(k1, "k_"))
}
