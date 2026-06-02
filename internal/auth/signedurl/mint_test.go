package signedurl_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/stretchr/testify/require"
)

type fakeActive struct {
	key signedurl.Key
	err error
}

func (f fakeActive) Active(context.Context) (signedurl.Key, error) { return f.key, f.err }

func TestMintRoundTrips(t *testing.T) {
	t.Parallel()
	key := []byte("test-key-do-not-use-in-production")
	src := fakeActive{key: signedurl.Key{KID: "k1", Secret: key}}
	aud := "https://e.example"

	res, err := signedurl.Mint(context.Background(), src, "/blob/bafyX", time.Hour, aud)
	require.NoError(t, err)
	require.Equal(t, "k1", res.KID)
	require.Greater(t, res.Exp, time.Now().Unix())

	// The minted URL verifies through the verifier with a matching Origin.
	u, err := url.Parse(res.URL)
	require.NoError(t, err)
	v := signedurl.NewVerifier(
		fakeKeys{keys: map[string]signedurl.Key{"k1": {KID: "k1", Secret: key}}},
		fakeRevs{},
	)
	d := v.Verify(context.Background(), signedurl.VerifyInput{
		Path: u.Path, Query: u.Query(), Origin: aud, Now: time.Now(),
	})
	require.True(t, d.OK, "minted URL should verify; code=%s", d.Code)
	require.Equal(t, "bafyX", d.CID)
}

func TestMintRejectsNonContentPath(t *testing.T) {
	t.Parallel()
	src := fakeActive{key: signedurl.Key{KID: "k1", Secret: []byte("k")}}
	_, err := signedurl.Mint(context.Background(), src, "/api/v1/admin/x", time.Hour, "https://e.example")
	require.ErrorIs(t, err, signedurl.ErrInvalidPath)

	require.True(t, signedurl.IsContentPath("/blob/bafyX"))
	require.True(t, signedurl.IsContentPath("/i/bafyX/p/thumb.webp"))
	require.False(t, signedurl.IsContentPath("/blob/"))
	require.False(t, signedurl.IsContentPath("/health"))
}
