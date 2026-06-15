package main

import (
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/stretchr/testify/require"
)

func TestResolveOperatorConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Uploads.PublicUploads = true
	cfg.TosURL = "https://from-file/tos"
	cfg.Auth.Paranoid = true
	cfg.Auth.IssuerURL = "https://file-issuer"
	cfg.Uploads.MaxUploadSizeBytes = 5

	// (a) file values used when env unset
	rc := resolveOperatorConfig(cfg, func(string) string { return "" })
	if !rc.PublicUploads || rc.TosURL != "https://from-file/tos" || !rc.Paranoid ||
		rc.AuthIssuerURL != "https://file-issuer" || rc.MaxUploadSizeBytes != 5 {
		t.Fatalf("file values not used: %+v", rc)
	}

	// (b) env overrides win when set
	env := map[string]string{
		"NOVA_PUBLIC_UPLOADS":        "false",
		"NOVA_TOS_URL":               "https://env/tos",
		"NOVA_PARANOID":              "false",
		"NOVA_AUTH_ISSUER_URL":       "https://env-issuer",
		"NOVA_MAX_UPLOAD_SIZE_BYTES": "9",
	}
	rc = resolveOperatorConfig(cfg, func(k string) string { return env[k] })
	if rc.PublicUploads || rc.TosURL != "https://env/tos" || rc.Paranoid ||
		rc.AuthIssuerURL != "https://env-issuer" || rc.MaxUploadSizeBytes != 9 {
		t.Fatalf("env did not override: %+v", rc)
	}

	// (c) nil cfg + env-only works (back-compat)
	rc = resolveOperatorConfig(nil, func(k string) string {
		if k == "NOVA_PUBLIC_UPLOADS" {
			return "true"
		}
		return ""
	})
	if !rc.PublicUploads || rc.MaxUploadSizeBytes != config.DefaultMaxUploadSizeBytes {
		t.Fatalf("nil cfg env-only path broken: %+v", rc)
	}
}

func TestResolveOperatorConfigAssemblyConcurrency(t *testing.T) {
	// yaml value honored when no env override
	cfg := &config.Config{}
	cfg.Uploads.MaxConcurrentAssembly = 16
	rc := resolveOperatorConfig(cfg, func(string) string { return "" })
	require.Equal(t, 16, rc.MaxConcurrentAssembly)

	// env overrides yaml
	rc = resolveOperatorConfig(cfg, func(k string) string {
		if k == "NOVA_MAX_CONCURRENT_ASSEMBLY" {
			return "4"
		}
		return ""
	})
	require.Equal(t, 4, rc.MaxConcurrentAssembly)

	// default when neither set
	rc = resolveOperatorConfig(nil, func(string) string { return "" })
	require.Equal(t, config.DefaultMaxConcurrentAssembly, rc.MaxConcurrentAssembly)
}

func TestApplyEnvOverridesAssemblyConcurrency(t *testing.T) {
	getenv := func(k string) string {
		if k == "NOVA_MAX_CONCURRENT_ASSEMBLY" {
			return "16"
		}
		return ""
	}
	c := &config.Config{}
	applyEnvOverridesTo(c, getenv)
	require.Equal(t, 16, c.Uploads.MaxConcurrentAssembly)

	pins := envPinnedKeys(getenv)
	_, ok := pins["uploads.max_concurrent_assembly"]
	require.True(t, ok)
}
