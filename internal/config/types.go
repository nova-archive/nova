// Package config models the operator.yaml configuration surface.
// The struct mirrors the v3.1 spec; field defaults and validation
// live in operator_yaml.go (loader) and paranoid.go (mode overrides).
package config

import "time"

// Config is the root of operator.yaml.
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
