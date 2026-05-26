package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ResolveSecret applies the v3.1 secret-loading precedence:
//
//  1. environment variable named `envKey` (if non-empty)
//  2. file at the path in env var `envFileKey` (if env is set and non-empty)
//  3. file at `defaultMountPath` (if it exists)
//
// Returns the secret value (trimmed of leading/trailing whitespace) or
// an error if no source resolves.
//
// This is the entry point for loading every operator secret in Phase 1:
// NOVA_MASTER_KEY, NOVA_OIDC_SIGNING_KEY, IPFS_SWARM_KEY, etc.
func ResolveSecret(envKey, envFileKey, defaultMountPath string) (string, error) {
	if v := os.Getenv(envKey); v != "" {
		return strings.TrimSpace(v), nil
	}

	if path := os.Getenv(envFileKey); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secrets: read %s (from $%s): %w", path, envFileKey, err)
		}
		return strings.TrimSpace(string(data)), nil
	}

	if defaultMountPath != "" {
		data, err := os.ReadFile(defaultMountPath)
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("secrets: read %s: %w", defaultMountPath, err)
		}
	}

	return "", fmt.Errorf("secrets: none of $%s, $%s, or %s resolved", envKey, envFileKey, defaultMountPath)
}
