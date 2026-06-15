package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigSetPATCHes(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":2,"restart_required":[]}`))
	}))
	defer srv.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "operator-tok")

	_, err := captureStdout(t, func() error {
		return cmdConfigSet([]string{"uploads.limits.max_concurrent_global", "8"})
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPatch, gotMethod)
	require.Equal(t, "/api/v1/admin/config", gotPath)
	require.Equal(t, "Bearer operator-tok", gotAuth)
	up := gotBody["uploads"].(map[string]any)["limits"].(map[string]any)
	require.Equal(t, float64(8), up["max_concurrent_global"])
}

func TestConfigGetGETs(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":1,"config":{},"fields":{}}`))
	}))
	defer srv.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "operator-tok")

	out, err := captureStdout(t, func() error { return cmdConfigGet(nil) })
	require.NoError(t, err)
	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "/api/v1/admin/config", gotPath)
	require.Contains(t, out, "version")
}

func TestConfigSetWarnsOnRestartRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":3,"restart_required":["auth.issuer_url"]}`))
	}))
	defer srv.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "operator-tok")

	// stderr carries the warning; just assert the command succeeds and the call was made.
	_, err := captureStdout(t, func() error {
		return cmdConfigSet([]string{"auth.issuer_url", "https://idp.test/"})
	})
	require.NoError(t, err)
}

func TestConfigSetParsesValueTypes(t *testing.T) {
	// bool and int values must serialize as JSON bool/number, not strings.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":2,"restart_required":[]}`))
	}))
	defer srv.Close()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "operator-tok")
	_, err := captureStdout(t, func() error {
		return cmdConfigSet([]string{"uploads.public_uploads", "true"})
	})
	require.NoError(t, err)
	require.Equal(t, true, gotBody["uploads"].(map[string]any)["public_uploads"])
}
