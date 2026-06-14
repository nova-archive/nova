package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadFromFile reads, parses, and validates an operator.yaml.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses operator.yaml from a byte slice.
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	cfg.privacyWarnings = ApplyPrivacyPreset(&cfg)
	applyUploadDefaults(&cfg)
	return &cfg, nil
}

// applyUploadDefaults fills zero-valued Uploads fields with the Default*
// constants so callers always see a usable write-path configuration.
func applyUploadDefaults(cfg *Config) {
	if cfg.Uploads.MaxUploadSizeBytes <= 0 {
		cfg.Uploads.MaxUploadSizeBytes = DefaultMaxUploadSizeBytes
	}
	if cfg.Uploads.SessionTTLSeconds <= 0 {
		cfg.Uploads.SessionTTLSeconds = DefaultUploadSessionTTLSecs
	}
	if cfg.Uploads.MaxConcurrentAssembly <= 0 {
		cfg.Uploads.MaxConcurrentAssembly = DefaultMaxConcurrentAssembly
	}
}

// validate enforces the v3.1 refuse-to-start floors and basic shape.
// (Per the spec, the coordinator runs the same validator at startup;
// validation lives here so the loader and the runtime use one code path.)
func validate(cfg *Config) error {
	if cfg.Operator.Hostname == "" {
		return fmt.Errorf("config: operator.hostname is required")
	}
	if cfg.Operator.ContactEmail == "" {
		return fmt.Errorf("config: operator.contact_email is required")
	}

	switch cfg.TLS.Mode {
	case "dev-self-signed", "http-01", "dns-01", "static", "onion":
		// ok
	case "":
		return fmt.Errorf("config: tls.mode is required (dev-self-signed|http-01|dns-01|static|onion)")
	default:
		return fmt.Errorf("config: tls.mode unknown: %q", cfg.TLS.Mode)
	}
	if cfg.TLS.Mode == "static" && (cfg.TLS.CertPath == "" || cfg.TLS.KeyPath == "") {
		return fmt.Errorf("config: tls.mode=static requires cert_path and key_path")
	}

	if cfg.Orchestrator.Replication.Factor.Important < 1 ||
		cfg.Orchestrator.Replication.Factor.Important > 20 {
		return fmt.Errorf("config: orchestrator.replication.factor.important out of range")
	}

	switch cfg.Moderation.TakedownDefaultAction {
	case "quarantine", "tombstone":
		// ok
	case "":
		// default is quarantine; allow empty for compactness
		cfg.Moderation.TakedownDefaultAction = "quarantine"
	default:
		return fmt.Errorf("config: moderation.takedown_default_action unknown")
	}

	if cfg.Auth.Anonymous && cfg.Moderation.TakedownDefaultAction == "" {
		// Coordinator's v3 floor: auth: anonymous AND moderation: off
		// is refused. moderation_off is currently encoded as
		// takedown_default_action being absent (i.e., no moderation
		// flow); future moderation.enabled field will refine this.
		return fmt.Errorf("config: auth.anonymous with no moderation flow is refused")
	}

	// T1.20: public uploads require operator to publish terms of service.
	if cfg.Uploads.PublicUploads && cfg.TosURL == "" {
		return fmt.Errorf("config: uploads.public_uploads requires tos_url (T1.20)")
	}

	return nil
}
