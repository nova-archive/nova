// Package secret implements Nova's secret-loading precedence. It is a
// stdlib-only leaf shared by the coordinator and the donor (nova-node); it
// imports no other Nova package so the donor dependency boundary stays clean.
package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Source identifies which precedence step satisfied a ResolveSecret call. The
// string form is stable and intended for startup log lines.
type Source string

const (
	SourceEnv     Source = "env"      // inline env value
	SourceFileEnv Source = "file_env" // path from the *_FILE env var
	SourceMount   Source = "mount"    // defaultMountPath
)

// ResolveSecret applies the precedence: (1) env var envKey; (2) file at the
// path in envFileKey; (3) file at defaultMountPath. Returns the trimmed secret
// value plus the resolving Source, or an error if none resolves.
func ResolveSecret(envKey, envFileKey, defaultMountPath string) (string, Source, error) {
	if v := os.Getenv(envKey); v != "" {
		return strings.TrimSpace(v), SourceEnv, nil
	}
	if path := os.Getenv(envFileKey); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("secret: read %s (from $%s): %w", path, envFileKey, err)
		}
		return strings.TrimSpace(string(data)), SourceFileEnv, nil
	}
	if defaultMountPath != "" {
		data, err := os.ReadFile(defaultMountPath)
		if err == nil {
			return strings.TrimSpace(string(data)), SourceMount, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("secret: read %s: %w", defaultMountPath, err)
		}
	}
	return "", "", fmt.Errorf("secret: none of $%s, $%s, or %s resolved", envKey, envFileKey, defaultMountPath)
}
