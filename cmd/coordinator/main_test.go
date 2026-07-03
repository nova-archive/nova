package main

import (
	"context"
	"errors"
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

func TestRunBothStopsOnFirstError(t *testing.T) {
	ctx := context.Background()
	good := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	bad := func(ctx context.Context) error { return errors.New("boom") }
	err := runBoth(ctx, bad, good)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom (and good must be cancelled)", err)
	}
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

// TestResolveMetricsListenAddr pins the D-M7-1 runtime resolution across BOTH
// config sources: env NOVA_METRICS_LISTEN_ADDR (LookupEnv — empty is a
// meaningful "disabled") wins over yaml; the yaml tri-state applies when the
// env is unset; the env-only deployment (opCfg == nil) gets the default.
func TestResolveMetricsListenAddr(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	cfgWith := func(v *string) *config.Config {
		return &config.Config{Coordinator: config.Coordinator{MetricsListenAddr: v}}
	}
	envWith := func(v string, set bool) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == "NOVA_METRICS_LISTEN_ADDR" && set {
				return v, true
			}
			return "", false
		}
	}
	cases := []struct {
		name    string
		cfg     *config.Config
		env     func(string) (string, bool)
		want    string
		enabled bool
	}{
		{"env empty disables", cfgWith(strPtr("127.0.0.1:8888")), envWith("", true), "", false},
		{"env addr wins over yaml", cfgWith(strPtr("127.0.0.1:8888")), envWith("127.0.0.1:9999", true), "127.0.0.1:9999", true},
		{"yaml empty disables", cfgWith(strPtr("")), envWith("", false), "", false},
		{"yaml addr", cfgWith(strPtr("127.0.0.1:8888")), envWith("", false), "127.0.0.1:8888", true},
		{"env-only deployment defaults", nil, envWith("", false), "127.0.0.1:2112", true},
		{"yaml key absent defaults", cfgWith(nil), envWith("", false), "127.0.0.1:2112", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, enabled := resolveMetricsListenAddr(c.cfg, c.env)
			require.Equal(t, c.want, addr)
			require.Equal(t, c.enabled, enabled)
		})
	}
}
