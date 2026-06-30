// Package config models the operator.yaml configuration surface.
// The struct mirrors the v3.1 spec; field defaults and validation
// live in operator_yaml.go (loader) and paranoid.go (mode overrides).
package config

import (
	"fmt"
	"time"
)

// Config is the root of operator.yaml.
// DeprecationWarnings returns one-time startup warnings for config that is still
// parsed but no longer does what it says (D-M5-2a). prune_stale_seconds is
// superseded by liveness status as the donor-source freshness authority: the M5
// sweeper, not a time window, decides sourceability.
func (c *Config) DeprecationWarnings() []string {
	var w []string
	if c.Coordinator.PruneStaleSeconds > 0 {
		w = append(w, "coordinator.prune_stale_seconds is deprecated and ignored (P2-M5): "+
			"donor-source freshness is now liveness status, not a time window")
	}
	return w
}

type Config struct {
	Operator       Operator       `yaml:"operator"`
	TLS            TLS            `yaml:"tls"`
	Auth           Auth           `yaml:"auth"`
	Orchestrator   Orchestrator   `yaml:"orchestrator"`
	Federation     Federation     `yaml:"federation"`
	IntegrityAudit IntegrityAudit `yaml:"integrity_audit"`
	Moderation     Moderation     `yaml:"moderation"`
	Coordinator    Coordinator    `yaml:"coordinator"`

	// Uploads tunes the M4 write path (tus + multipart).
	Uploads Uploads `yaml:"uploads"`

	// SignedURLs tunes the M7 signed-URL verifier/rotation/minting. Phase 1
	// reads these from NOVA_SIGNED_URL_* env in cmd/coordinator; the loader
	// maps this section once operator.yaml is wired in.
	SignedURLs SignedURLs `yaml:"signed_urls,omitempty"`

	// MasterKeyRotation tunes the M10 re-wrap worker. Zero-valued fields take the
	// documented defaults (4 workers, 256 ids/batch, 50ms inter-batch pace).
	MasterKeyRotation MasterKeyRotation `yaml:"master_key_rotation,omitempty"`

	// Webhook destinations; honored only when paranoid=false.
	Webhooks []WebhookDestination `yaml:"webhooks,omitempty"`

	// SourceIPRetentionDays default per spec is 30; 1 in paranoid mode.
	SourceIPRetentionDays int `yaml:"source_ip_retention_days,omitempty"`

	// TosURL must be set when public uploads are enabled.
	TosURL string `yaml:"tos_url,omitempty"`

	// PossessionAudit tunes the P2-M6 donor possession-proof challenge loop.
	// Zero-valued fields default via Effective* accessors; Validate() allows zero
	// (means "unset") and rejects only invalid explicit values.
	PossessionAudit PossessionAudit `yaml:"possession_audit,omitempty"`

	// privacyWarnings holds consequence warnings produced by ApplyPrivacyPreset
	// at load time (e.g. paranoid on but webhooks configured). Unexported so it
	// is never (de)serialized; read via PrivacyWarnings().
	privacyWarnings []string
}

type Operator struct {
	Hostname     string `yaml:"hostname"`
	ContactEmail string `yaml:"contact_email"`
	DisplayName  string `yaml:"display_name,omitempty"`
}

type TLS struct {
	// Mode: one of "dev-self-signed", "http-01", "dns-01", "static", "onion".
	Mode string `yaml:"mode"`
	// For "static" mode:
	CertPath string `yaml:"cert_path,omitempty"`
	KeyPath  string `yaml:"key_path,omitempty"`
}

type Auth struct {
	// Empty string = use the built-in local OIDC issuer.
	// Non-empty = external OIDC provider URL.
	IssuerURL    string   `yaml:"issuer_url"`
	ClientID     string   `yaml:"client_id,omitempty"`
	Scopes       []string `yaml:"scopes,omitempty"`
	JWKSCacheTTL int      `yaml:"jwks_cache_ttl_seconds,omitempty"`
	Paranoid     bool     `yaml:"paranoid"`
	// Anonymous=true is refused in production builds (refuse-to-start floor).
	// Allowed only with the nova_dev build tag.
	Anonymous bool `yaml:"anonymous,omitempty"`

	// ClientSecretFile is the path to the OIDC client-secret file.
	ClientSecretFile string `yaml:"client_secret_file,omitempty"`
	// RoleClaim is the JWT claim used to extract roles; defaults to "groups".
	RoleClaim string `yaml:"role_claim,omitempty"`
	// RoleMapping maps IdP groups/scopes to nova roles.
	RoleMapping map[string]string `yaml:"role_mapping,omitempty"`
}

type Orchestrator struct {
	TickIntervalSeconds        int         `yaml:"tick_interval_seconds"`
	StepSeconds                int         `yaml:"step_seconds"`
	Replication                Replication `yaml:"replication"`
	MassCasualtyThresholdRatio float64     `yaml:"mass_casualty_threshold_ratio"`
	MassCasualtyWindowSeconds  int         `yaml:"mass_casualty_window_seconds"`
	CapacityRunwayFloorDays    int         `yaml:"capacity_runway_floor_days"`
	// ReputationFloor excludes donors below this reputation from healing placement
	// + re-replicates below-floor acked pins (M5, D-M5-7). Defaults to
	// DefaultReputationFloor when non-positive.
	ReputationFloor float64 `yaml:"reputation_floor,omitempty"`
}

// DefaultReputationFloor is the M5 healing reputation cutoff.
const DefaultReputationFloor = 0.5

// EffectiveReputationFloor returns the configured floor, defaulting when unset.
func (o Orchestrator) EffectiveReputationFloor() float64 {
	if o.ReputationFloor <= 0 {
		return DefaultReputationFloor
	}
	return o.ReputationFloor
}

type Replication struct {
	Factor ReplicationFactor `yaml:"factor"`
}

type ReplicationFactor struct {
	Important int `yaml:"important"`
	Normal    int `yaml:"normal"`
	Cache     int `yaml:"cache"`
}

type Federation struct {
	// M2 additions: listener + mTLS material.
	ListenAddr         string `yaml:"listen_addr"`
	NebulaInterface    string `yaml:"nebula_interface"`
	FederationCAPath   string `yaml:"federation_ca_path"`
	FederationCertPath string `yaml:"federation_cert_path"`
	FederationKeyPath  string `yaml:"federation_key_path"`

	// Existing timers.
	HeartbeatIntervalSeconds     int `yaml:"heartbeat_interval_seconds"`
	PinsPollIntervalSeconds      int `yaml:"pins_poll_interval_seconds"`
	MaxPinConcurrency            int `yaml:"max_pin_concurrency"`
	ChangeLogRetentionHours      int `yaml:"change_log_retention_hours"`
	SuspectAfterMissedHeartbeats int `yaml:"suspect_after_missed_heartbeats"`
	UnreachableAfterSeconds      int `yaml:"unreachable_after_seconds"`
	EvictedAfterSeconds          int `yaml:"evicted_after_seconds"`

	// M4: repair-token + transfer.
	RepairTokenTTLSeconds int    `yaml:"repair_token_ttl_seconds"`
	RepairSigningKeyPath  string `yaml:"repair_signing_key_path"`
	MaxTransferBytes      int64  `yaml:"max_transfer_bytes"`
	SourceNebulaAddr      string `yaml:"source_nebula_addr"`

	// M4.1: coordinator-as-client mTLS identity for donor read-source endpoints.
	// FederationClientCertPath is the path to the nova://coordinator/<uuid> client
	// cert PEM. FederationClientKeyPath is the path to its private key (or empty
	// to use NOVA_FEDERATION_CLIENT_KEY[_FILE] env chain). Omitting both causes
	// graceful degradation: donor-fetch is disabled until the operator provisions
	// the identity (Task 7 consumer).
	FederationClientCertPath string `yaml:"federation_client_cert_path"`
	FederationClientKeyPath  string `yaml:"federation_client_key_path"`

	// M4.1: donor read-path containment knobs (Task 8). The donor-backed read
	// tier treats each fetch as a protected integration point. All are
	// zero-defaulted by the accessor methods below; an operator rarely needs to
	// touch them. /settings wiring is Task 14.
	ReadSourceTimeoutSeconds      int `yaml:"read_source_timeout_seconds"`           // per-holder fetch+read timeout (default 30)
	ReadSourceBulkhead            int `yaml:"read_source_bulkhead"`                  // coordinator-wide max concurrent donor fetches (default 16)
	ReadSourcePerDonorLimit       int `yaml:"read_source_per_donor_fetch_limit"`     // max concurrent fetches to one donor (default 4)
	ReadSourceBreakerThreshold    int `yaml:"read_source_breaker_threshold"`         // consecutive failures before a donor breaker opens (default 5)
	ReadSourceBreakerCooldownSecs int `yaml:"read_source_breaker_cooldown_seconds"`  // breaker half-open delay (default 30)
	ReadSourceMaxFallbacks        int `yaml:"read_source_max_fallbacks_per_request"` // max donor fetch ATTEMPTS per request (default 3)
}

// Enabled reports whether the federation listener should run (operator set a
// listen_addr).
func (f Federation) Enabled() bool { return f.ListenAddr != "" }

// IntegrityAudit mirrors the operator.yaml integrity_audit section. Phase 1
// (M8) consumes the INTEGRITY_AUDIT.md schedule defaults as code constants via
// coordinator.Config (integrity.DefaultCadences); the operator.yaml loader maps
// this section once it is wired into cmd.
type IntegrityAudit struct {
	EnvelopeDecode            AuditCadence `yaml:"envelope_decode"`
	KeyUnwrap                 AuditCadence `yaml:"key_unwrap"`
	SampleDecrypt             AuditCadence `yaml:"sample_decrypt"`
	KuboPinPresent            AuditCadence `yaml:"kubo_pin_present"`
	DerivativeStateConsistent AuditCadence `yaml:"derivative_state_consistent"`
	BlockHashValid            AuditCadence `yaml:"block_hash_valid"`
	ManifestConsistent        AuditCadence `yaml:"manifest_consistent"`
}

type AuditCadence struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	SampleSize      int `yaml:"sample_size"`
}

// PossessionAudit tunes the P2-M6 donor possession-proof challenge loop.
// All fields are zero-defaulted via Effective* accessors; operators rarely
// need to touch them. Task 13 wires these into the audit scheduler.
type PossessionAudit struct {
	BaseIntervalSeconds int     `yaml:"base_interval_seconds,omitempty"`
	DeadlineSeconds     int     `yaml:"deadline_seconds,omitempty"`
	AuditBudgetFraction float64 `yaml:"audit_budget_fraction,omitempty"`
	MaxBlockBytes       int64   `yaml:"max_block_bytes,omitempty"`
	MinAgeDays          int     `yaml:"min_age_days,omitempty"`
	MinPassedAudits     int64   `yaml:"min_passed_audits,omitempty"`
	MinAckedTransfers   int64   `yaml:"min_acked_transfers,omitempty"`
	GraduateReputation  float64 `yaml:"graduate_reputation,omitempty"`
}

// Default constants for PossessionAudit zero-value accessors.
const (
	DefaultPossessionBaseInterval      = 3600 * time.Second
	DefaultPossessionDeadline          = 30 * time.Second
	DefaultPossessionAuditBudget       = 0.01
	DefaultPossessionMaxBlockBytes     = int64(262144)
	DefaultPossessionMinAge            = 7 * 24 * time.Hour
	DefaultPossessionGraduateRep       = 0.95
	DefaultPossessionMinPassedAudits   = int64(10)
	DefaultPossessionMinAckedTransfers = int64(5)
)

// EffectiveBaseInterval returns the configured base challenge interval, defaulting to 1 hour.
func (p PossessionAudit) EffectiveBaseInterval() time.Duration {
	if p.BaseIntervalSeconds <= 0 {
		return DefaultPossessionBaseInterval
	}
	return time.Duration(p.BaseIntervalSeconds) * time.Second
}

// EffectiveDeadline returns the configured per-challenge deadline, defaulting to 30s.
func (p PossessionAudit) EffectiveDeadline() time.Duration {
	if p.DeadlineSeconds <= 0 {
		return DefaultPossessionDeadline
	}
	return time.Duration(p.DeadlineSeconds) * time.Second
}

// EffectiveAuditBudgetFraction returns the configured audit budget fraction, defaulting to 0.01.
func (p PossessionAudit) EffectiveAuditBudgetFraction() float64 {
	if p.AuditBudgetFraction <= 0 {
		return DefaultPossessionAuditBudget
	}
	return p.AuditBudgetFraction
}

// EffectiveMaxBlockBytes returns the configured max challenge block size, defaulting to 256 KiB.
func (p PossessionAudit) EffectiveMaxBlockBytes() int64 {
	if p.MaxBlockBytes <= 0 {
		return DefaultPossessionMaxBlockBytes
	}
	return p.MaxBlockBytes
}

// EffectiveMinAge returns the configured minimum pin age before auditing, defaulting to 7 days.
func (p PossessionAudit) EffectiveMinAge() time.Duration {
	if p.MinAgeDays <= 0 {
		return DefaultPossessionMinAge
	}
	return time.Duration(p.MinAgeDays) * 24 * time.Hour
}

// EffectiveGraduateRep returns the reputation threshold for graduating a donor
// from audit-heavy to audit-light, defaulting to 0.95.
func (p PossessionAudit) EffectiveGraduateRep() float64 {
	if p.GraduateReputation <= 0 {
		return DefaultPossessionGraduateRep
	}
	return p.GraduateReputation
}

// EffectiveMinPassedAudits returns the minimum passed-audit count before
// graduation, defaulting to 10.
func (p PossessionAudit) EffectiveMinPassedAudits() int64 {
	if p.MinPassedAudits <= 0 {
		return DefaultPossessionMinPassedAudits
	}
	return p.MinPassedAudits
}

// EffectiveMinAckedTransfers returns the minimum acked-transfer count before
// graduation, defaulting to 5.
func (p PossessionAudit) EffectiveMinAckedTransfers() int64 {
	if p.MinAckedTransfers <= 0 {
		return DefaultPossessionMinAckedTransfers
	}
	return p.MinAckedTransfers
}

// Validate returns an error for invalid explicit values. Zero means "unset"
// (the Effective* accessors supply defaults), so a wholly-zero struct is valid.
func (p PossessionAudit) Validate() error {
	if p.AuditBudgetFraction != 0 && (p.AuditBudgetFraction < 0 || p.AuditBudgetFraction > 1) {
		return fmt.Errorf("config: possession_audit.audit_budget_fraction must be in [0,1], got %v", p.AuditBudgetFraction)
	}
	if p.DeadlineSeconds < 0 {
		return fmt.Errorf("config: possession_audit.deadline_seconds must be >= 0, got %d", p.DeadlineSeconds)
	}
	if p.BaseIntervalSeconds < 0 {
		return fmt.Errorf("config: possession_audit.base_interval_seconds must be >= 0, got %d", p.BaseIntervalSeconds)
	}
	if p.MaxBlockBytes < 0 {
		return fmt.Errorf("config: possession_audit.max_block_bytes must be >= 0, got %d", p.MaxBlockBytes)
	}
	if p.GraduateReputation != 0 && (p.GraduateReputation < 0 || p.GraduateReputation > 1) {
		return fmt.Errorf("config: possession_audit.graduate_reputation must be in [0,1], got %v", p.GraduateReputation)
	}
	if p.MinAgeDays < 0 {
		return fmt.Errorf("config: possession_audit.min_age_days must be >= 0, got %d", p.MinAgeDays)
	}
	if p.MinPassedAudits < 0 {
		return fmt.Errorf("config: possession_audit.min_passed_audits must be >= 0, got %d", p.MinPassedAudits)
	}
	if p.MinAckedTransfers < 0 {
		return fmt.Errorf("config: possession_audit.min_acked_transfers must be >= 0, got %d", p.MinAckedTransfers)
	}
	return nil
}

type Moderation struct {
	TakedownDefaultAction       string `yaml:"takedown_default_action"`
	DMCACounterNotificationDays int    `yaml:"dmca_counter_notification_days"`
}

type Coordinator struct {
	PublicIpfsDht bool `yaml:"public_ipfs_dht"`

	// RecordSourceIP controls whether blobs.source_ip is recorded. Tri-state
	// pointer: nil = operator did not set it (effective default: record; the
	// paranoid preset fills it false). An explicit value always wins over the
	// preset — see ApplyPrivacyPreset and docs/PRIVACY_AUDIT.md.
	RecordSourceIP *bool `yaml:"record_source_ip,omitempty"`

	// CoordinatorStorageMode (P2-M4.1, D-M4.1-9) selects how the coordinator
	// treats donor-fetched blobs on the read path:
	//   - "origin_copy" (default): keep every donor-fetched blob pinned; the
	//     coordinator is a full origin copy and never cache-evicts.
	//   - "bounded_cache": cap locally-cached donor-fetched bytes at
	//     BoundedCacheMaxBytes via a size-aware SLRU/2Q policy (probationary →
	//     protected on a second access), so a scan over Nova's large immutable
	//     blobs cannot pollute the hot set.
	//   - "transient": hold nothing beyond the in-flight read; unpin after each
	//     read so the next committed read is donor-backed again.
	// An empty/unknown value normalizes to "origin_copy".
	CoordinatorStorageMode string `yaml:"coordinator_storage_mode,omitempty"`

	// BoundedCacheMaxBytes is the bounded_cache total byte budget (probationary +
	// protected). Honored only when coordinator_storage_mode="bounded_cache"; a
	// non-positive value disables eviction (treated as unbounded — operator
	// misconfiguration). All byte accounting is by envelope_size (D-M4.1-16).
	BoundedCacheMaxBytes int64 `yaml:"bounded_cache_max_bytes,omitempty"`

	// BoundedCacheProtectedRatio caps the protected segment at
	// ProtectedRatio × BoundedCacheMaxBytes so the probationary tier always
	// retains admission headroom. Default 0.80; values ≤0 or >1 normalize to the
	// default.
	BoundedCacheProtectedRatio float64 `yaml:"bounded_cache_protected_ratio,omitempty"`

	// BoundedCacheMaxObjectBytes refuses cache admission of any single object
	// larger than this (the bytes are still served, just not cached). 0 = no
	// per-object ceiling.
	BoundedCacheMaxObjectBytes int64 `yaml:"bounded_cache_max_object_bytes,omitempty"`

	// LruTouchIntervalSeconds throttles last_accessed_at bumps and protected
	// promotions: a second access within this window is a no-op so hot reads do
	// not churn the DB. Default 60s; non-positive normalizes to the default.
	LruTouchIntervalSeconds int `yaml:"lru_touch_interval_seconds,omitempty"`

	// RequireReplicationQuorumBeforeCommit (P2-M4.1, D-M4.1-11) is the opt-in
	// async durability gate. false (default): an upload commits immediately and
	// is publicly readable at once (today's behavior). true: an upload returns
	// 202 with the blob in 'staging' (not publicly readable); a background
	// reconciler flips it to 'committed' once a live acked-holder quorum exists,
	// and only then fires the product OnCommitted hook. Staging-write is NOT a
	// durable commit; see docs and pkg/coordinator/storage/reconciler.go.
	RequireReplicationQuorumBeforeCommit bool `yaml:"require_replication_quorum_before_commit,omitempty"`

	// CommitQuorum is the live-acked sourceable-holder count needed to commit a
	// staging blob, per durability class. Honored only when
	// RequireReplicationQuorumBeforeCommit=true. Zero-valued fields take the
	// DefaultCommitQuorum* constants (Important=2, Normal=2, Cache=1) via
	// applyCommitGateDefaults.
	CommitQuorum ReplicationFactor `yaml:"commit_quorum,omitempty"`

	// CommitReconcilerIntervalSeconds is how often the durability reconciler
	// scans for staging blobs whose quorum may now be met. Default 30s;
	// non-positive normalizes to the default via the accessor.
	CommitReconcilerIntervalSeconds int `yaml:"commit_reconciler_interval_seconds,omitempty"`

	// CommitFailAfterSeconds is the staging-age ceiling: a staging blob older
	// than this whose quorum is still unmet is marked 'failed' (a permanent miss
	// — the upload never reached durability). Default 3600s (1h); non-positive
	// normalizes to the default via the accessor.
	CommitFailAfterSeconds int `yaml:"commit_fail_after_seconds,omitempty"`

	// PruneSafetyFloor is the minimum live acked DONOR holder count required
	// before an origin blob (important/normal) may be pruned (unpinned + marked
	// absent) from the coordinator. If the live holder count is < Floor, the
	// local copy is retained. Default 2. Honored only in bounded_cache and
	// transient storage modes (origin_copy never prunes).
	PruneSafetyFloor int `yaml:"prune_safety_floor,omitempty"`

	// PrunerIntervalSeconds is how often the origin pruner scans for prune-
	// eligible blobs. Default 60s; non-positive normalizes to the default via
	// the accessor.
	PrunerIntervalSeconds int `yaml:"pruner_interval_seconds,omitempty"`

	// PruneStaleSeconds is the donor freshness window for CountSourceableHolders
	// in the pruner: donors last-seen older than this are excluded (treated as
	// not live). Default 3600s; mirrors the federation donor-freshness window.
	// Non-positive normalizes to the default via the accessor.
	PruneStaleSeconds float64 `yaml:"prune_stale_seconds,omitempty"`
}

// Default coordinator_storage_mode tunables (P2-M4.1). Mirror the storage-layer
// defaults in pkg/coordinator/storage/cache.go; both must stay in sync.
const (
	// DefaultCoordinatorStorageMode is the full-origin-copy posture (never evict).
	DefaultCoordinatorStorageMode = "origin_copy"
	// DefaultBoundedCacheProtectedRatio caps the protected segment at 80% of the
	// bounded-cache budget.
	DefaultBoundedCacheProtectedRatio = 0.80
	// DefaultLruTouchIntervalSeconds throttles cache touch/promote DB writes.
	DefaultLruTouchIntervalSeconds = 60
)

// StorageMode returns the configured coordinator_storage_mode, normalized to
// DefaultCoordinatorStorageMode when empty/unknown.
func (c Coordinator) StorageMode() string {
	switch c.CoordinatorStorageMode {
	case "origin_copy", "bounded_cache", "transient":
		return c.CoordinatorStorageMode
	default:
		return DefaultCoordinatorStorageMode
	}
}

// BoundedCacheProtectedRatioOrDefault returns the protected-segment ratio,
// normalized to DefaultBoundedCacheProtectedRatio when ≤0 or >1.
func (c Coordinator) BoundedCacheProtectedRatioOrDefault() float64 {
	if c.BoundedCacheProtectedRatio <= 0 || c.BoundedCacheProtectedRatio > 1 {
		return DefaultBoundedCacheProtectedRatio
	}
	return c.BoundedCacheProtectedRatio
}

// LruTouchInterval returns the cache touch/promote throttle window, defaulting
// to DefaultLruTouchIntervalSeconds when non-positive.
func (c Coordinator) LruTouchInterval() time.Duration {
	s := c.LruTouchIntervalSeconds
	if s <= 0 {
		s = DefaultLruTouchIntervalSeconds
	}
	return time.Duration(s) * time.Second
}

// Commit-gate defaults (P2-M4.1, D-M4.1-11). The CommitQuorum defaults are the
// minimum live acked sourceable-holder count per class; they are intentionally
// modest (a coordinator-as-source deployment reaches them quickly) and are
// applied by applyCommitGateDefaults before validation. The interval/fail-after
// defaults are applied lazily by the accessors below.
const (
	// DefaultCommitQuorumImportant is the live-acked-holder count needed to
	// commit a user-uploaded original under the gate.
	DefaultCommitQuorumImportant = 2
	// DefaultCommitQuorumNormal is the quorum for regenerable derivatives.
	DefaultCommitQuorumNormal = 2
	// DefaultCommitQuorumCache is the quorum for transient artifacts.
	DefaultCommitQuorumCache = 1
	// DefaultCommitReconcilerIntervalSeconds is the reconciler scan cadence.
	DefaultCommitReconcilerIntervalSeconds = 30
	// DefaultCommitFailAfterSeconds is the staging-age ceiling before a
	// quorum-starved blob is marked failed (1 hour).
	DefaultCommitFailAfterSeconds = 3600
)

// CommitReconcilerInterval returns the reconciler scan cadence, defaulting to
// DefaultCommitReconcilerIntervalSeconds when non-positive.
func (c Coordinator) CommitReconcilerInterval() time.Duration {
	s := c.CommitReconcilerIntervalSeconds
	if s <= 0 {
		s = DefaultCommitReconcilerIntervalSeconds
	}
	return time.Duration(s) * time.Second
}

// CommitFailAfter returns the staging-age ceiling, defaulting to
// DefaultCommitFailAfterSeconds when non-positive.
func (c Coordinator) CommitFailAfter() time.Duration {
	s := c.CommitFailAfterSeconds
	if s <= 0 {
		s = DefaultCommitFailAfterSeconds
	}
	return time.Duration(s) * time.Second
}

// Pruner defaults (P2-M4.1, D-M4.1-12). Applied lazily by the accessors below.
const (
	// DefaultPruneSafetyFloor is the minimum live acked DONOR holder count
	// required before an origin blob may be pruned. Conservative default.
	DefaultPruneSafetyFloor = 2
	// DefaultPrunerIntervalSeconds is the pruner scan cadence.
	DefaultPrunerIntervalSeconds = 60
	// DefaultPruneStaleSeconds is the donor freshness window for the pruner's
	// CountSourceableHolders call; mirrors the federation staleness default.
	DefaultPruneStaleSeconds = 3600.0
)

// PruneSafetyFloorOrDefault returns the prune_safety_floor, defaulting to
// DefaultPruneSafetyFloor when non-positive.
func (c Coordinator) PruneSafetyFloorOrDefault() int {
	if c.PruneSafetyFloor <= 0 {
		return DefaultPruneSafetyFloor
	}
	return c.PruneSafetyFloor
}

// PrunerInterval returns the pruner scan cadence, defaulting to
// DefaultPrunerIntervalSeconds when non-positive.
func (c Coordinator) PrunerInterval() time.Duration {
	s := c.PrunerIntervalSeconds
	if s <= 0 {
		s = DefaultPrunerIntervalSeconds
	}
	return time.Duration(s) * time.Second
}

// PruneStale returns the donor freshness window for the pruner, defaulting to
// DefaultPruneStaleSeconds when non-positive.
func (c Coordinator) PruneStale() float64 {
	if c.PruneStaleSeconds <= 0 {
		return DefaultPruneStaleSeconds
	}
	return c.PruneStaleSeconds
}

type WebhookDestination struct {
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"`
	Secret string   `yaml:"secret_file,omitempty"`
}

// Upload-pipeline defaults (M4). The size ceiling is an artificial Phase-1
// limit tied to V1 whole-object encryption; Phase-2 streaming AEAD lifts it.
const (
	// DefaultMaxUploadSizeBytes is 100 MiB.
	DefaultMaxUploadSizeBytes int64 = 104857600
	// DefaultUploadSessionTTLSecs is 24 hours.
	DefaultUploadSessionTTLSecs = 86400
	// DefaultMaxConcurrentAssembly bounds concurrent in-memory assembly.
	DefaultMaxConcurrentAssembly = 8
	// DefaultMaxConcurrentGlobalUploads bounds total concurrent tus uploads.
	DefaultMaxConcurrentGlobalUploads = 16
	// DefaultMaxConcurrentPerSession bounds concurrent uploads per session.
	DefaultMaxConcurrentPerSession = 4
	// DefaultMaxFilesPerSession bounds the number of files per upload session.
	DefaultMaxFilesPerSession = 100
)

// Replication-factor defaults (per content class). Applied by the loader
// (applyReplicationDefaults) before validation so a config that omits the
// section still gets durable defaults. The important-class default errs high
// for durability of irreplaceable originals; see docs/specs/HEALING_PROTOCOL.md.
const (
	// DefaultReplicationImportant is the donor replica count for user-uploaded
	// originals. High by default; lowering it is warn-not-force.
	DefaultReplicationImportant = 5
	// DefaultReplicationNormal is the donor replica count for regenerable
	// derivatives.
	DefaultReplicationNormal = 3
	// DefaultReplicationCache is the donor replica count for transient artifacts.
	DefaultReplicationCache = 2
)

// Uploads configures the M4 write path. Zero-valued fields are filled with the
// Default* constants by the loader (applyUploadDefaults).
type Uploads struct {
	MaxUploadSizeBytes    int64  `yaml:"max_upload_size_bytes"`
	SessionTTLSeconds     int    `yaml:"session_ttl_seconds"`
	MaxConcurrentAssembly int    `yaml:"max_concurrent_assembly"`
	TmpDir                string `yaml:"tmp_dir"`
	// PublicUploads allows unauthenticated uploads; requires tos_url (T1.20).
	PublicUploads bool `yaml:"public_uploads,omitempty"`

	// CORS configures cross-origin resource sharing for the upload endpoint.
	// Disabled by default; enabling it applies spec tus defaults for methods/headers.
	CORS CORS `yaml:"cors"`
	// Limits caps concurrent and per-session upload activity.
	Limits UploadLimits `yaml:"limits,omitempty"`
	// DefaultCollectionID, when set, is the collection that uploads with no
	// explicit collection (and no token-bound collection) join. Point it at a
	// public collection (see `novactl collection create`) to make anonymous
	// widget uploads publicly viewable without per-upload wiring. Empty = none
	// (uploads with no collection resolve to private). Live-reloadable.
	DefaultCollectionID string `yaml:"default_collection_id,omitempty"`
}

// CORS configures the Access-Control-* response headers for the upload endpoint.
type CORS struct {
	Enabled          bool     `yaml:"enabled"`
	AllowedOrigins   []string `yaml:"allowed_origins,omitempty"`
	AllowedMethods   []string `yaml:"allowed_methods,omitempty"`
	AllowedHeaders   []string `yaml:"allowed_headers,omitempty"`
	ExposedHeaders   []string `yaml:"exposed_headers,omitempty"`
	AllowCredentials bool     `yaml:"allow_credentials,omitempty"`
}

// UploadLimits caps concurrent and per-session upload activity.
type UploadLimits struct {
	MaxConcurrentGlobal     int `yaml:"max_concurrent_global,omitempty"`
	MaxConcurrentPerSession int `yaml:"max_concurrent_per_session,omitempty"`
	MaxFilesPerSession      int `yaml:"max_files_per_session,omitempty"`
}

// SignedURLs configures the M7 signed-URL verifier/rotation/minting. Defaults
// (applied in cmd/coordinator): 86400s grace, 30s revocation refresh, 60s key
// cache, 86400s max mint ttl.
type SignedURLs struct {
	GraceWindowSeconds       int `yaml:"grace_window_seconds,omitempty"`
	RevocationRefreshSeconds int `yaml:"revocation_refresh_seconds,omitempty"`
	KeyCacheTTLSeconds       int `yaml:"key_cache_ttl_seconds,omitempty"`
	MaxTTLSeconds            int `yaml:"max_ttl_seconds,omitempty"`
}

// MasterKeyRotation tunes the M10 re-wrap worker. Zero-valued fields take the
// documented defaults (4 workers, 256 ids/batch, 50ms inter-batch pace).
type MasterKeyRotation struct {
	RewrapConcurrency int           `yaml:"rewrap_concurrency"`
	RewrapBatchSize   int           `yaml:"rewrap_batch_size"`
	RewrapPace        time.Duration `yaml:"rewrap_pace"`
}
