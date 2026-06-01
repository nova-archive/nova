package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretSource identifies which precedence step satisfied a ResolveSecret
// call. The string form is stable and is intended for inclusion in startup
// log lines so operators can confirm whether `v1` came from an inline env,
// a NOVA_FOO_FILE redirect, or the default secret-mount path.
type SecretSource string

const (
	SourceEnv     SecretSource = "env"      // NOVA_FOO inline value
	SourceFileEnv SecretSource = "file_env" // NOVA_FOO_FILE path
	SourceMount   SecretSource = "mount"    // defaultMountPath
)

// ResolveSecret applies the v3.1 secret-loading precedence:
//
//  1. environment variable named `envKey` (if non-empty)
//  2. file at the path in env var `envFileKey` (if env is set and non-empty)
//  3. file at `defaultMountPath` (if it exists)
//
// Returns the secret value (trimmed of leading/trailing whitespace) plus a
// SecretSource indicating which step resolved, or an error if no source
// resolves.
//
// This is the entry point for loading every operator secret in Phase 1:
// NOVA_MASTER_KEY_<LABEL>, NOVA_OIDC_SIGNING_KEY, etc. M6.2 B5 added the
// SecretSource return value so callers can log the resolution source at
// startup without echoing the secret bytes themselves.
func ResolveSecret(envKey, envFileKey, defaultMountPath string) (string, SecretSource, error) {
	if v := os.Getenv(envKey); v != "" {
		return strings.TrimSpace(v), SourceEnv, nil
	}

	if path := os.Getenv(envFileKey); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("secrets: read %s (from $%s): %w", path, envFileKey, err)
		}
		return strings.TrimSpace(string(data)), SourceFileEnv, nil
	}

	if defaultMountPath != "" {
		data, err := os.ReadFile(defaultMountPath)
		if err == nil {
			return strings.TrimSpace(string(data)), SourceMount, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("secrets: read %s: %w", defaultMountPath, err)
		}
	}

	return "", "", fmt.Errorf("secrets: none of $%s, $%s, or %s resolved", envKey, envFileKey, defaultMountPath)
}
