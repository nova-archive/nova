package signedurl_test

import (
	"bufio"
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/stretchr/testify/require"
)

// --- fakes -----------------------------------------------------------------

type fakeKeys struct{ keys map[string]signedurl.Key }

func (f fakeKeys) ByKID(_ context.Context, kid string) (signedurl.Key, error) {
	k, ok := f.keys[kid]
	if !ok {
		return signedurl.Key{}, signedurl.ErrUnknownKID
	}
	return k, nil
}

type fakeRevs struct {
	revoked  map[string]bool // "kind:value"
	prefixes []string
}

func (f fakeRevs) IsRevoked(cid, aud, kid, path string) bool {
	if f.revoked["cid:"+cid] || f.revoked["aud:"+aud] || f.revoked["kid:"+kid] {
		return true
	}
	for _, p := range f.prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// --- canonical + sign ------------------------------------------------------

func TestCanonical(t *testing.T) {
	t.Parallel()
	require.Equal(t, "/blob/bafy/0\n0\n\n", signedurl.Canonical("/blob/bafy/0", 0, "", ""))
	require.Equal(t,
		"/i/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/p/thumb.webp\n1730000000\nhttps://example.com\nk_2026_05",
		signedurl.Canonical("/i/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi/p/thumb.webp", 1730000000, "https://example.com", "k_2026_05"))
}

func TestSignNoPadding(t *testing.T) {
	t.Parallel()
	require.NotContains(t, signedurl.Sign([]byte("k"), "anything"), "=")
}

// TestVectors checks Sign against independently-generated reference vectors.
func TestVectors(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/vectors.txt")
	require.NoError(t, err)
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		require.Len(t, parts, 3, "vector line: %q", line)
		canonical, err := strconv.Unquote(parts[1])
		require.NoError(t, err)
		require.Equal(t, parts[2], signedurl.Sign([]byte(parts[0]), canonical), "canonical=%q", canonical)
		n++
	}
	require.NoError(t, sc.Err())
	require.GreaterOrEqual(t, n, 3)
}

// --- verify ----------------------------------------------------------------

func TestVerify(t *testing.T) {
	t.Parallel()
	key := []byte("test-key-do-not-use-in-production")
	kid := "k1"
	keys := fakeKeys{keys: map[string]signedurl.Key{kid: {KID: kid, Secret: key}}}
	now := time.Unix(1_700_000_000, 0)
	path := "/blob/bafyexamplecid"
	aud := "https://e.example"
	exp := now.Unix() + 3600
	goodSig := signedurl.Sign(key, signedurl.Canonical(path, exp, aud, kid))

	base := func() signedurl.VerifyInput {
		q := url.Values{}
		q.Set("sig", goodSig)
		q.Set("exp", strconv.FormatInt(exp, 10))
		q.Set("aud", aud)
		q.Set("kid", kid)
		return signedurl.VerifyInput{Path: path, Query: q, Origin: aud, Now: now}
	}
	verify := func(in signedurl.VerifyInput, revs signedurl.Revocations) signedurl.Decision {
		if revs == nil {
			revs = fakeRevs{}
		}
		return signedurl.NewVerifier(keys, revs).Verify(context.Background(), in)
	}

	t.Run("valid", func(t *testing.T) {
		d := verify(base(), nil)
		require.True(t, d.OK, "code=%s", d.Code)
		require.Equal(t, "bafyexamplecid", d.CID)
		require.Equal(t, kid, d.Kid)
	})
	t.Run("missing_param", func(t *testing.T) {
		in := base()
		in.Query.Del("kid")
		require.Equal(t, signedurl.CodeMissingParam, verify(in, nil).Code)
	})
	t.Run("malformed_sig_is_missing_param", func(t *testing.T) {
		in := base()
		in.Query.Set("sig", "!!!not-base64!!!")
		require.Equal(t, signedurl.CodeMissingParam, verify(in, nil).Code)
	})
	t.Run("unknown_kid", func(t *testing.T) {
		in := base()
		in.Query.Set("kid", "kX")
		require.Equal(t, signedurl.CodeUnknownKID, verify(in, nil).Code)
	})
	t.Run("revoked_cid", func(t *testing.T) {
		require.Equal(t, signedurl.CodeRevoked, verify(base(), fakeRevs{revoked: map[string]bool{"cid:bafyexamplecid": true}}).Code)
	})
	t.Run("revoked_aud", func(t *testing.T) {
		require.Equal(t, signedurl.CodeRevoked, verify(base(), fakeRevs{revoked: map[string]bool{"aud:" + aud: true}}).Code)
	})
	t.Run("revoked_kid", func(t *testing.T) {
		require.Equal(t, signedurl.CodeRevoked, verify(base(), fakeRevs{revoked: map[string]bool{"kid:" + kid: true}}).Code)
	})
	t.Run("revoked_path_prefix", func(t *testing.T) {
		require.Equal(t, signedurl.CodeRevoked, verify(base(), fakeRevs{prefixes: []string{"/blob/bafy"}}).Code)
	})
	t.Run("path_prefix_miss", func(t *testing.T) {
		require.True(t, verify(base(), fakeRevs{prefixes: []string{"/i/"}}).OK)
	})
	t.Run("expired_past", func(t *testing.T) {
		pexp := now.Unix() - 1
		in := base()
		in.Query.Set("exp", strconv.FormatInt(pexp, 10))
		in.Query.Set("sig", signedurl.Sign(key, signedurl.Canonical(path, pexp, aud, kid)))
		require.Equal(t, signedurl.CodeExpired, verify(in, nil).Code)
	})
	t.Run("expired_at_now_zero_skew", func(t *testing.T) {
		in := base()
		in.Query.Set("exp", strconv.FormatInt(now.Unix(), 10))
		in.Query.Set("sig", signedurl.Sign(key, signedurl.Canonical(path, now.Unix(), aud, kid)))
		require.Equal(t, signedurl.CodeExpired, verify(in, nil).Code)
	})
	t.Run("valid_at_now_plus_one", func(t *testing.T) {
		e := now.Unix() + 1
		in := base()
		in.Query.Set("exp", strconv.FormatInt(e, 10))
		in.Query.Set("sig", signedurl.Sign(key, signedurl.Canonical(path, e, aud, kid)))
		require.True(t, verify(in, nil).OK)
	})
	t.Run("invalid_sig", func(t *testing.T) {
		in := base()
		bad := []byte(goodSig)
		if bad[0] == 'A' {
			bad[0] = 'B'
		} else {
			bad[0] = 'A'
		}
		in.Query.Set("sig", string(bad))
		require.Equal(t, signedurl.CodeInvalid, verify(in, nil).Code)
	})
	t.Run("aud_mismatch", func(t *testing.T) {
		in := base()
		in.Origin = "https://evil.example"
		require.Equal(t, signedurl.CodeAudMismatch, verify(in, nil).Code)
	})
	t.Run("referer_fallback", func(t *testing.T) {
		in := base()
		in.Origin = ""
		in.Referer = aud + "/some/page?x=1"
		require.True(t, verify(in, nil).OK)
	})
	t.Run("no_origin_no_referer", func(t *testing.T) {
		in := base()
		in.Origin = ""
		require.Equal(t, signedurl.CodeAudMismatch, verify(in, nil).Code)
	})
}
