package signedurl_test

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/stretchr/testify/require"
)

type fakeRevQ struct {
	rows  []gen.ListRevocationsRow
	calls int
}

func (f *fakeRevQ) ListRevocations(_ context.Context) ([]gen.ListRevocationsRow, error) {
	f.calls++
	return f.rows, nil
}

func TestRevocationsIsRevoked(t *testing.T) {
	t.Parallel()
	q := &fakeRevQ{rows: []gen.ListRevocationsRow{
		{Kind: "cid", Value: "bafyX"},
		{Kind: "aud", Value: "https://evil.example"},
		{Kind: "kid", Value: "k_leaked"},
		{Kind: "path_prefix", Value: "/i/bafyP/"},
	}}
	r := signedurl.NewRevocations(q)
	require.NoError(t, r.Load(context.Background()))

	require.True(t, r.IsRevoked("bafyX", "", "", "/blob/bafyX"))
	require.True(t, r.IsRevoked("", "https://evil.example", "", "/blob/y"))
	require.True(t, r.IsRevoked("", "", "k_leaked", "/blob/y"))
	require.True(t, r.IsRevoked("", "", "", "/i/bafyP/p/thumb.webp"))

	require.False(t, r.IsRevoked("bafyOther", "https://ok.example", "k_ok", "/i/bafyOther/x"))
	require.False(t, r.IsRevoked("", "", "", "/i/bafyPnot"), "prefix requires the trailing slash boundary")
}

func TestRevocationsInvalidatePicksUpNewRows(t *testing.T) {
	t.Parallel()
	q := &fakeRevQ{}
	r := signedurl.NewRevocations(q)
	require.NoError(t, r.Load(context.Background()))
	require.False(t, r.IsRevoked("bafyNew", "", "", "/blob/bafyNew"))

	q.rows = []gen.ListRevocationsRow{{Kind: "cid", Value: "bafyNew"}}
	require.NoError(t, r.Invalidate(context.Background()))
	require.True(t, r.IsRevoked("bafyNew", "", "", "/blob/bafyNew"))
}

func TestRevocationsEmptyBeforeLoad(t *testing.T) {
	t.Parallel()
	r := signedurl.NewRevocations(&fakeRevQ{})
	require.False(t, r.IsRevoked("x", "y", "z", "/blob/x"))
}
