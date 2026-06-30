package config

import (
	"fmt"
	"log/slog"
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

	// Replication factors: all three in [1,20] (D-M5-7a). important is irreplaceable
	// user-uploaded originals — R=1 makes "one failure from permanent loss" the
	// steady state and defeats Tier-1, so important<2 is REFUSED. normal/cache are
	// regenerable (derivatives / transient artifacts), so R=1 is warn-not-force.
	rf := cfg.Orchestrator.Replication.Factor
	for _, c := range []struct {
		name string
		v    int
	}{{"important", rf.Important}, {"normal", rf.Normal}, {"cache", rf.Cache}} {
		if c.v < 1 || c.v > 20 {
			return fmt.Errorf("config: orchestrator.replication.factor.%s=%d out of range [1,20]", c.name, c.v)
		}
	}
	if rf.Important < 2 {
		return fmt.Errorf("config: orchestrator.replication.factor.important must be >= 2 " +
			"(R=1 means one failure from permanent loss of irreplaceable originals)")
	}
	if rf.Normal < 2 {
		slog.Warn("config: orchestrator.replication.factor.normal < 2; a single failure loses the only copy of a derivative until it is regenerated",
			"normal", rf.Normal)
	}
	if rf.Cache < 2 {
		slog.Warn("config: orchestrator.replication.factor.cache < 2; transient artifacts have no redundancy",
			"cache", rf.Cache)
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

	if err := validateStorageRead(cfg); err != nil {
		return err
	}

	if err := cfg.PossessionAudit.Validate(); err != nil {
		return err
	}

	return nil
}

// validateStorageRead enforces the M4.1 storage/read redirect safety rules.
// Hard errors (refuse to start) are returned as errors; advisory violations
// are emitted as slog.Warn and do not prevent startup.
func validateStorageRead(cfg *Config) error {
	mode := cfg.Coordinator.CoordinatorStorageMode
	gate := cfg.Coordinator.RequireReplicationQuorumBeforeCommit

	// --- REFUSE: unknown coordinator_storage_mode (explicit non-empty, non-valid value) ---
	switch mode {
	case "", "origin_copy", "bounded_cache", "transient":
		// ok (empty normalises to origin_copy)
	default:
		return fmt.Errorf("config: coordinator_storage_mode unknown: %q (want origin_copy|bounded_cache|transient)", mode)
	}

	// --- REFUSE: transient without a commit quorum gate ---
	if mode == "transient" && !gate {
		return fmt.Errorf("config: coordinator_storage_mode=transient requires require_replication_quorum_before_commit=true (no local copy; committing without a donor quorum risks data with no durable copy)")
	}

	// --- REFUSE: bounded_cache_protected_ratio explicitly out of (0,1) ---
	if r := cfg.Coordinator.BoundedCacheProtectedRatio; r != 0 && (r <= 0 || r >= 1) {
		return fmt.Errorf("config: bounded_cache_protected_ratio must be in (0,1); got %g", r)
	}

	// --- REFUSE: per-object ceiling above whole-cache budget ---
	if cfg.Coordinator.BoundedCacheMaxObjectBytes > 0 &&
		cfg.Coordinator.BoundedCacheMaxBytes > 0 &&
		cfg.Coordinator.BoundedCacheMaxObjectBytes > cfg.Coordinator.BoundedCacheMaxBytes {
		return fmt.Errorf("config: bounded_cache_max_object_bytes (%d) > bounded_cache_max_bytes (%d); per-object ceiling cannot exceed whole-cache budget",
			cfg.Coordinator.BoundedCacheMaxObjectBytes, cfg.Coordinator.BoundedCacheMaxBytes)
	}

	// Gate/quorum range checks are always applied (even when gate=false) so
	// operators catch nonsensical quorum values at load time rather than
	// discovering them silently when they enable the gate later.
	rf := cfg.Orchestrator.Replication.Factor
	cq := cfg.Coordinator.CommitQuorum
	floor := cfg.Coordinator.PruneSafetyFloorOrDefault()

	type classRow struct {
		name   string
		factor int
		quorum int
	}
	classes := []classRow{
		{"important", rf.Important, cq.Important},
		{"normal", rf.Normal, cq.Normal},
		{"cache", rf.Cache, cq.Cache},
	}

	for _, class := range classes {
		q := class.quorum
		f := class.factor
		// REFUSE: quorum out of [1, factor]
		if q < 1 || q > f {
			return fmt.Errorf("config: commit_quorum.%s=%d is out of range [1,%d] (replication.factor.%s=%d)",
				class.name, q, f, class.name, f)
		}

		// Prunable classes only: important and normal
		if class.name == "cache" {
			continue
		}

		// REFUSE: floor < quorum (pruning below the commit quorum is unsafe)
		if floor < q {
			return fmt.Errorf("config: prune_safety_floor=%d is below commit_quorum.%s=%d; pruning below the quorum that made a blob committable is unsafe",
				floor, class.name, q)
		}
		// WARN: floor > factor (the floor can never be reached; pruning will never happen)
		if floor > f {
			slog.Warn("config: prune_safety_floor exceeds replication factor; origin pruning will never trigger",
				"prune_safety_floor", floor,
				"replication_factor_"+class.name, f,
				"class", class.name,
			)
		}
	}

	// --- WARN: bounded_cache mode without the commit gate ---
	if mode == "bounded_cache" && !gate {
		slog.Warn("config: coordinator_storage_mode=bounded_cache without require_replication_quorum_before_commit; origin pruning will run without a durability gate — consider enabling the gate")
	}

	// --- WARN: bounded_cache budget smaller than the max single-transfer size ---
	if mode == "bounded_cache" && cfg.Coordinator.BoundedCacheMaxBytes > 0 {
		maxXfer := cfg.Federation.MaxTransfer()
		if cfg.Coordinator.BoundedCacheMaxBytes < maxXfer {
			slog.Warn("config: bounded_cache_max_bytes is smaller than federation.max_transfer_bytes; objects at the transfer size limit cannot be cached and the cache will thrash or refuse them",
				"bounded_cache_max_bytes", cfg.Coordinator.BoundedCacheMaxBytes,
				"max_transfer_bytes", maxXfer,
			)
		}
	}

	// --- WARN: donor freshness window too tight relative to heartbeat cadence ---
	hb := cfg.Federation.HeartbeatIntervalSeconds
	if hb <= 0 {
		hb = DefaultHeartbeatIntervalSeconds
	}
	stale := cfg.Federation.SourceStaleSeconds()
	if stale < 2*float64(hb) {
		slog.Warn("config: federation.source_stale_seconds is less than 2× heartbeat_interval_seconds; transient poll gaps may empty the sourceable donor set",
			"source_stale_seconds", stale,
			"heartbeat_interval_seconds", hb,
		)
	}

	return nil
}
