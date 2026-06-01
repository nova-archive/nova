package httputil_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/stretchr/testify/require"
)

func TestClientIP_NoTrustedProxies_IgnoresXFF(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	got := httputil.ClientIP(r, nil)
	require.Equal(t, "203.0.113.7", got.String(),
		"with no trusted proxies, XFF must be ignored even if present")
}

func TestClientIP_TrustedProxy_HonorsXFF(t *testing.T) {
	t.Parallel()
	trusted, err := httputil.ParseTrustedProxies("127.0.0.1/32")
	require.NoError(t, err)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.1")
	got := httputil.ClientIP(r, trusted)
	require.Equal(t, "1.2.3.4", got.String(),
		"behind a trusted proxy, the leftmost XFF hop is honored")
}

func TestClientIP_UntrustedRemote_IgnoresXFF(t *testing.T) {
	t.Parallel()
	trusted, err := httputil.ParseTrustedProxies("127.0.0.1/32")
	require.NoError(t, err)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.7:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	got := httputil.ClientIP(r, trusted)
	require.Equal(t, "203.0.113.7", got.String(),
		"untrusted remote: XFF must be ignored even with trusted-proxy list configured")
}

func TestClientIP_TrustedProxy_NoXFFFallsBackToRemote(t *testing.T) {
	t.Parallel()
	trusted, _ := httputil.ParseTrustedProxies("127.0.0.1/32")
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	got := httputil.ClientIP(r, trusted)
	require.Equal(t, "127.0.0.1", got.String())
}

func TestClientIP_TrustedCIDR_HonorsXFF(t *testing.T) {
	t.Parallel()
	trusted, err := httputil.ParseTrustedProxies("10.0.0.0/8")
	require.NoError(t, err)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.5.1.2:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	got := httputil.ClientIP(r, trusted)
	require.Equal(t, "1.2.3.4", got.String())
}

func TestClientIP_InvalidXFF_FallsBackToRemote(t *testing.T) {
	t.Parallel()
	trusted, _ := httputil.ParseTrustedProxies("127.0.0.1/32")
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	got := httputil.ClientIP(r, trusted)
	require.Equal(t, "127.0.0.1", got.String())
}

func TestClientIPString_InvalidRemoteAddr_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "garbage"
	require.Equal(t, "", httputil.ClientIPString(r, nil))
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    []string // string form of prefixes
		wantErr bool
	}{
		{"", nil, false},
		{"   ", nil, false},
		{"127.0.0.1", []string{"127.0.0.1/32"}, false},
		{"10.0.0.0/8", []string{"10.0.0.0/8"}, false},
		{"127.0.0.1,10.0.0.0/8,::1", []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128"}, false},
		{" 127.0.0.1 , 10.0.0.0/8 ", []string{"127.0.0.1/32", "10.0.0.0/8"}, false},
		{"not-an-ip", nil, true},
		{"10.0.0.0/33", nil, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			got, err := httputil.ParseTrustedProxies(c.in)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			gotStrs := make([]string, 0, len(got))
			for _, p := range got {
				gotStrs = append(gotStrs, p.String())
			}
			if c.want == nil {
				require.Empty(t, gotStrs)
			} else {
				require.Equal(t, c.want, gotStrs)
			}
		})
	}
}

// Sanity test that ParsePrefix on a bare IP is rejected (so the helper
// correctly fans out to PrefixFrom). Documents intent for future readers.
func TestParseTrustedProxies_BareIPGetsExplicitPrefix(t *testing.T) {
	t.Parallel()
	got, err := httputil.ParseTrustedProxies("203.0.113.7")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, netip.MustParseAddr("203.0.113.7"), got[0].Addr())
	require.Equal(t, 32, got[0].Bits())
}
