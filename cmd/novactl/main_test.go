package main

import (
	"os"
	"path/filepath"
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
