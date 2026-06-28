package config

import (
	"fmt"
	"os"

	"github.com/google/uuid"
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
	// Replication defaults are applied before validation: validate() range-checks
	// the important factor, so an omitted section must be defaulted first (durable
	// R by default rather than a hard "out of range" error).
	applyReplicationDefaults(&cfg)
	applyCommitGateDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	cfg.privacyWarnings = ApplyPrivacyPreset(&cfg)
	applyUploadDefaults(&cfg)
	return &cfg, nil
}

// applyReplicationDefaults fills zero-valued replication factors with the
// Default* constants so a config that omits orchestrator.replication still gets
// durable defaults. Runs before validate() because the validator range-checks
// the important factor. Lowering important below the default is permitted here
// (validation enforces only the 1..20 range); the warn-not-force notice is
// emitted by the orchestrator when it consumes R (P2-M5). See
// docs/specs/HEALING_PROTOCOL.md.
func applyReplicationDefaults(cfg *Config) {
	if cfg.Orchestrator.Replication.Factor.Important <= 0 {
		cfg.Orchestrator.Replication.Factor.Important = DefaultReplicationImportant
	}
	if cfg.Orchestrator.Replication.Factor.Normal <= 0 {
		cfg.Orchestrator.Replication.Factor.Normal = DefaultReplicationNormal
	}
	if cfg.Orchestrator.Replication.Factor.Cache <= 0 {
		cfg.Orchestrator.Replication.Factor.Cache = DefaultReplicationCache
	}
}

// applyCommitGateDefaults fills zero-valued coordinator commit-quorum factors
// with the DefaultCommitQuorum* constants so the gate (when enabled) always has
// a usable per-class quorum. The gate itself stays off unless
// require_replication_quorum_before_commit is set; defaulting the quorum
// unconditionally keeps the consumed config self-consistent regardless. The
// interval/fail-after knobs default lazily in their accessors. P2-M4.1.
func applyCommitGateDefaults(cfg *Config) {
	if cfg.Coordinator.CommitQuorum.Important <= 0 {
		cfg.Coordinator.CommitQuorum.Important = DefaultCommitQuorumImportant
	}
	if cfg.Coordinator.CommitQuorum.Normal <= 0 {
		cfg.Coordinator.CommitQuorum.Normal = DefaultCommitQuorumNormal
	}
	if cfg.Coordinator.CommitQuorum.Cache <= 0 {
		cfg.Coordinator.CommitQuorum.Cache = DefaultCommitQuorumCache
	}
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
	if cfg.Uploads.Limits.MaxConcurrentGlobal <= 0 {
		cfg.Uploads.Limits.MaxConcurrentGlobal = DefaultMaxConcurrentGlobalUploads
	}
	if cfg.Uploads.Limits.MaxConcurrentPerSession <= 0 {
		cfg.Uploads.Limits.MaxConcurrentPerSession = DefaultMaxConcurrentPerSession
	}
	if cfg.Uploads.Limits.MaxFilesPerSession <= 0 {
		cfg.Uploads.Limits.MaxFilesPerSession = DefaultMaxFilesPerSession
	}
	// When CORS is enabled, fill in spec tus defaults for methods/headers only
	// if the operator has not provided their own lists.
	if cfg.Uploads.CORS.Enabled {
		if len(cfg.Uploads.CORS.AllowedMethods) == 0 {
			cfg.Uploads.CORS.AllowedMethods = []string{
				"GET", "POST", "PATCH", "HEAD", "DELETE", "OPTIONS",
			}
		}
		if len(cfg.Uploads.CORS.AllowedHeaders) == 0 {
			cfg.Uploads.CORS.AllowedHeaders = []string{
				"Authorization", "Content-Type", "Tus-Resumable",
				"Upload-Length", "Upload-Offset", "Upload-Metadata",
			}
		}
		if len(cfg.Uploads.CORS.ExposedHeaders) == 0 {
			cfg.Uploads.CORS.ExposedHeaders = []string{
				"Location", "Upload-Offset", "Tus-Resumable",
				"X-Nova-Cid", "X-Nova-Envelope-Version",
			}
		}
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

	if s := cfg.Uploads.DefaultCollectionID; s != "" {
		if _, err := uuid.Parse(s); err != nil {
			return fmt.Errorf("config: uploads.default_collection_id must be a uuid: %w", err)
		}
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
