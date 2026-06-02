package signedurl_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

func TestGuard(t *testing.T) {
	t.Parallel()
	key := []byte("test-key-do-not-use-in-production")
	kid := "k1"
	v := signedurl.NewVerifier(
		fakeKeys{keys: map[string]signedurl.Key{kid: {KID: kid, Secret: key}}},
		fakeRevs{},
	)

	const path = "/blob/bafyX"
	const aud = "https://e.example"

	signedQuery := func(exp int64, sig string) url.Values {
		q := url.Values{}
		q.Set("sig", sig)
		q.Set("exp", strconv.FormatInt(exp, 10))
		q.Set("aud", aud)
		q.Set("kid", kid)
		return q
	}
	req := func(q url.Values, origin string) *http.Request {
		target := path
		if e := q.Encode(); e != "" {
			target += "?" + e
		}
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	t.Run("passthrough_no_params", func(t *testing.T) {
		var granted bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			granted = storage.ReadAuthorized(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		v.Guard(next).ServeHTTP(rec, req(url.Values{}, ""))
		require.Equal(t, http.StatusOK, rec.Code)
		require.False(t, granted, "passthrough must not grant private access")
	})

	t.Run("valid_grants_private_read", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).Unix()
		sig := signedurl.Sign(key, signedurl.Canonical(path, exp, aud, kid))
		var granted bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			granted = storage.ReadAuthorized(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})
		rec := httptest.NewRecorder()
		v.Guard(next).ServeHTTP(rec, req(signedQuery(exp, sig), aud))
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.True(t, granted, "valid signature must grant private access")
	})

	t.Run("invalid_403_and_next_not_called", func(t *testing.T) {
		exp := time.Now().Add(time.Hour).Unix()
		good := signedurl.Sign(key, signedurl.Canonical(path, exp, aud, kid))
		bad := []byte(good)
		if bad[0] == 'A' {
			bad[0] = 'B'
		} else {
			bad[0] = 'A'
		}
		called := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
		rec := httptest.NewRecorder()
		v.Guard(next).ServeHTTP(rec, req(signedQuery(exp, string(bad)), aud))
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.False(t, called, "next must not run on a bad signature")
		require.Contains(t, rec.Body.String(), signedurl.CodeInvalid)
	})

	t.Run("expired_403", func(t *testing.T) {
		exp := time.Now().Add(-time.Second).Unix()
		sig := signedurl.Sign(key, signedurl.Canonical(path, exp, aud, kid))
		rec := httptest.NewRecorder()
		v.Guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
			ServeHTTP(rec, req(signedQuery(exp, sig), aud))
		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Contains(t, rec.Body.String(), signedurl.CodeExpired)
	})
}
