package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteCredentialsIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentials(path, credentials{BaseURL: "https://nova/", AccessToken: "a", RefreshToken: "r", KID: "k"}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestCredsPathHonorsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := credsPath()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "nova", "credentials.json"), p)
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	cmdErr := fn()
	require.NoError(t, w.Close())
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), cmdErr
}

func TestSignedURLSignPrintsURL(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url": "/blob/bafyX?aud=https%3A%2F%2Fe.example&exp=2000000000&kid=k1&sig=abc",
			"kid": "k1", "exp": 2000000000,
		})
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	credPath, err := credsPath()
	require.NoError(t, err)
	require.NoError(t, writeCredentials(credPath, credentials{BaseURL: srv.URL, AccessToken: "tok"}))

	out, err := captureStdout(t, func() error {
		return cmdSignedURLSign([]string{"--path", "/blob/bafyX", "--ttl", "300", "--aud", "https://e.example"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/signed-urls/sign", gotPath)
	require.Equal(t, "Bearer tok", gotAuth)
	require.Contains(t, gotBody, `"path":"/blob/bafyX"`)
	require.Contains(t, gotBody, `"ttl_seconds":300`)
	require.Contains(t, strings.TrimSpace(out), srv.URL+"/blob/bafyX?")
}

func TestSignedURLSignRequiresFlags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdSignedURLSign([]string{"--path", "/blob/bafyX"}) // missing --aud
	require.Error(t, err)
	require.Contains(t, err.Error(), "required")
}

func TestSignedURLSignNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdSignedURLSign([]string{"--path", "/blob/bafyX", "--aud", "https://e.example"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}
