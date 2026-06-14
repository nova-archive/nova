package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --------------------------------------------------------------------------
// Unit tests: expiresAtFromDuration
// --------------------------------------------------------------------------

func TestExpiresAtFromDurationEmpty(t *testing.T) {
	got, err := expiresAtFromDuration("", time.Now())
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestExpiresAtFromDurationValid(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := expiresAtFromDuration("720h", now)
	require.NoError(t, err)
	// 720h = 30 days from 2026-01-01 = 2026-01-31T00:00:00Z
	require.Equal(t, "2026-01-31T00:00:00Z", got)
}

func TestExpiresAtFromDurationInvalid(t *testing.T) {
	_, err := expiresAtFromDuration("30d", time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --expires duration")
}

func TestExpiresAtFromDurationInvalidGarbage(t *testing.T) {
	_, err := expiresAtFromDuration("notaduration", time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --expires duration")
}

// --------------------------------------------------------------------------
// Unit tests: revoke arg validation
// --------------------------------------------------------------------------

func TestUploadTokenRevokeMissingID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenRevoke([]string{"--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "<id>")
}

func TestUploadTokenRevokeIDLooksLikeFlag(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenRevoke([]string{"-something"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "<id>")
}

// --------------------------------------------------------------------------
// Integration tests: upload-token create
// --------------------------------------------------------------------------

func TestUploadTokenCreatePOSTs(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tok-1","token":"nova_ut_abc","role":"upload_token","label":"my-label","created_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "operator-tok")

	out, err := captureStdout(t, func() error {
		return cmdUploadTokenCreate([]string{"--label", "my-label", "--product", "image", "--max-file-size", "1048576"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/upload-tokens", gotPath)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "Bearer operator-tok", gotAuth)
	require.Equal(t, "my-label", gotBody["label"])
	require.Equal(t, "image", gotBody["product"])
	require.Equal(t, float64(1048576), gotBody["max_file_size"])
	require.Contains(t, out, "nova_ut_abc")
	// Pretty-printed JSON check
	require.Contains(t, out, "\n")
}

func TestUploadTokenCreateWithExpires(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tok-2","token":"nova_ut_xyz","role":"upload_token","created_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "op-tok")

	_, err := captureStdout(t, func() error {
		return cmdUploadTokenCreate([]string{"--expires", "24h"})
	})
	require.NoError(t, err)
	expiresAt, ok := gotBody["expires_at"].(string)
	require.True(t, ok, "expires_at must be present in request body")
	require.True(t, strings.HasSuffix(expiresAt, "Z"), "expires_at must be UTC RFC3339")
}

func TestUploadTokenCreateOmitsEmptyFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"tok-3","token":"nova_ut_min","role":"upload_token","created_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "op-tok")

	_, err := captureStdout(t, func() error {
		return cmdUploadTokenCreate([]string{})
	})
	require.NoError(t, err)
	_, hasLabel := gotBody["label"]
	_, hasCollection := gotBody["collection_id"]
	_, hasProduct := gotBody["product"]
	_, hasMaxFileSize := gotBody["max_file_size"]
	_, hasExpiresAt := gotBody["expires_at"]
	require.False(t, hasLabel, "label must be omitted when not provided")
	require.False(t, hasCollection, "collection_id must be omitted when not provided")
	require.False(t, hasProduct, "product must be omitted when not provided")
	require.False(t, hasMaxFileSize, "max_file_size must be omitted when not provided")
	require.False(t, hasExpiresAt, "expires_at must be omitted when not provided")
}

func TestUploadTokenCreateInvalidDuration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenCreate([]string{"--expires", "30d"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --expires duration")
}

func TestUploadTokenCreate401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "expired")

	err := cmdUploadTokenCreate([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

func TestUploadTokenCreate403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "viewer-tok")

	err := cmdUploadTokenCreate([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forbidden")
}

func TestUploadTokenCreateNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenCreate([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}

// --------------------------------------------------------------------------
// Integration tests: upload-token list
// --------------------------------------------------------------------------

func TestUploadTokenListGETs(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"tok-1","role":"upload_token","created_at":"2026-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "op-tok")

	out, err := captureStdout(t, func() error {
		return cmdUploadTokenList([]string{})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/upload-tokens", gotPath)
	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "Bearer op-tok", gotAuth)
	require.Contains(t, out, "tok-1")
	// Pretty-printed JSON check
	require.Contains(t, out, "\n")
}

func TestUploadTokenList401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "expired")

	err := cmdUploadTokenList([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

func TestUploadTokenList403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "viewer-tok")

	err := cmdUploadTokenList([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forbidden")
}

func TestUploadTokenListNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenList([]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}

// --------------------------------------------------------------------------
// Integration tests: upload-token revoke
// --------------------------------------------------------------------------

func TestUploadTokenRevokeDELETEs(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "op-tok")

	out, err := captureStdout(t, func() error {
		return cmdUploadTokenRevoke([]string{"tok-123", "--no-confirm"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/upload-tokens/tok-123", gotPath)
	require.Equal(t, http.MethodDelete, gotMethod)
	require.Equal(t, "Bearer op-tok", gotAuth)
	require.Contains(t, out, "revoked")
}

func TestUploadTokenRevoke404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "op-tok")

	err := cmdUploadTokenRevoke([]string{"no-such-id", "--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found or already revoked")
}

func TestUploadTokenRevoke401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "expired")

	err := cmdUploadTokenRevoke([]string{"tok-1", "--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

func TestUploadTokenRevoke403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "viewer-tok")

	err := cmdUploadTokenRevoke([]string{"tok-1", "--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forbidden")
}

func TestUploadTokenRevokeNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdUploadTokenRevoke([]string{"tok-1", "--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}
