package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nova-archive/nova/internal/api/handlers"
)

func writeDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"), []byte("<!doctype html><title>nova admin</title>"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dist, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "assets", "app-abc123.js"), []byte("console.log('nova')"), 0o644))
	return dist
}

func TestAdminSPANilWhenUnset(t *testing.T) {
	require.Nil(t, handlers.NewAdminSPA(""))
}

func TestAdminSPAServesIndexAssetsAndFallback(t *testing.T) {
	h := handlers.NewAdminSPA(writeDist(t))
	require.NotNil(t, h)

	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Serve(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}

	// Root → index.html, no-store, strict CSP.
	root := get("/admin/")
	require.Equal(t, 200, root.Code)
	require.Contains(t, root.Body.String(), "nova admin")
	require.Equal(t, "no-store", root.Header().Get("Cache-Control"))
	require.Contains(t, root.Header().Get("Content-Security-Policy"), "default-src 'self'")
	require.NotContains(t, root.Header().Get("Content-Security-Policy"), "http://")
	require.Equal(t, "nosniff", root.Header().Get("X-Content-Type-Options"))

	// /admin (no trailing slash) also serves the index.
	require.Equal(t, 200, get("/admin").Code)

	// Deep link → SPA fallback (index.html), not 404.
	deep := get("/admin/blobs/bafy123")
	require.Equal(t, 200, deep.Code)
	require.Contains(t, deep.Body.String(), "nova admin")
	require.Equal(t, "no-store", deep.Header().Get("Cache-Control"))

	// Hashed asset → its bytes, immutable cache.
	asset := get("/admin/assets/app-abc123.js")
	require.Equal(t, 200, asset.Code)
	require.Contains(t, asset.Body.String(), "console.log")
	require.Contains(t, asset.Header().Get("Cache-Control"), "immutable")

	// A missing asset path falls back to index.html (SPA), never a directory listing.
	require.Contains(t, get("/admin/assets/").Body.String(), "nova admin")
}

func TestAdminSPATraversalCannotEscape(t *testing.T) {
	// Seed a secret OUTSIDE dist; a traversal attempt must never serve it.
	parent := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("TOP SECRET"), 0o644))
	dist := filepath.Join(parent, "dist")
	require.NoError(t, os.MkdirAll(dist, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"), []byte("<title>nova admin</title>"), 0o644))

	h := handlers.NewAdminSPA(dist)
	rec := httptest.NewRecorder()
	// Build the request with a raw escaping path that bypasses URL cleaning.
	r := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	r.URL.Path = "/admin/../secret.txt"
	h.Serve(rec, r)
	require.NotContains(t, rec.Body.String(), "TOP SECRET", "traversal must not escape dist")
}
