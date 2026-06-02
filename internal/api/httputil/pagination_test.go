package httputil_test

import (
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/stretchr/testify/require"
)

func TestParsePageDefaults(t *testing.T) {
	p, err := httputil.ParsePage(httptest.NewRequest("GET", "/x", nil))
	require.NoError(t, err)
	require.Equal(t, httputil.Page{Page: 1, PerPage: 50, Limit: 50, Offset: 0}, p)
}

func TestParsePageOffsetAndCap(t *testing.T) {
	p, err := httputil.ParsePage(httptest.NewRequest("GET", "/x?page=3&per_page=20", nil))
	require.NoError(t, err)
	require.Equal(t, httputil.Page{Page: 3, PerPage: 20, Limit: 20, Offset: 40}, p)

	capped, err := httputil.ParsePage(httptest.NewRequest("GET", "/x?per_page=500", nil))
	require.NoError(t, err)
	require.Equal(t, 100, capped.PerPage)
	require.Equal(t, 100, capped.Limit)
}

func TestParsePageRejectsInvalid(t *testing.T) {
	for _, q := range []string{"/x?page=0", "/x?page=-1", "/x?page=abc", "/x?per_page=0", "/x?per_page=xyz"} {
		_, err := httputil.ParsePage(httptest.NewRequest("GET", q, nil))
		require.Error(t, err, q)
	}
}
