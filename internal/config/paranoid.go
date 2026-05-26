package config

// ApplyParanoid mutates cfg to enforce paranoid-mode overrides per
// docs/PRIVACY_AUDIT.md § "paranoid: true".
//
// Called automatically from the loader after validate(); operators
// don't invoke it directly.
func ApplyParanoid(cfg *Config) {
	if !cfg.Auth.Paranoid {
		return
	}

	// Drop all outbound webhook destinations regardless of config.
	cfg.Webhooks = nil

	// Cap source-IP retention to 1 day.
	if cfg.SourceIPRetentionDays > 1 || cfg.SourceIPRetentionDays == 0 {
		cfg.SourceIPRetentionDays = 1
	}

	// TLS auto-renewal is operator-disabled in paranoid mode; certbot
	// is not part of the config schema (it's a separate compose service),
	// but the coordinator emits a startup warning if certbot is wired up
	// in paranoid mode. That check lives in cmd/coordinator at startup.

	// OpenTelemetry and Prometheus public-bound endpoints are refused
	// in paranoid mode; that's enforced at coordinator startup, not in
	// config validation, because the relevant fields aren't in
	// operator.yaml yet (Phase 1 ships Prometheus loopback-only; M14
	// adds OpenTelemetry config gating).
}
