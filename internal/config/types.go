// Package config models the operator.yaml configuration surface.
// The struct mirrors the v3.1 spec; field defaults and validation
// live in operator_yaml.go (loader) and paranoid.go (mode overrides).
package config

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

	// Webhook destinations; honored only when paranoid=false.
	Webhooks []WebhookDestination `yaml:"webhooks,omitempty"`

	// SourceIPRetentionDays default per spec is 30; 1 in paranoid mode.
	SourceIPRetentionDays int `yaml:"source_ip_retention_days,omitempty"`

	// TosURL must be set when public uploads are enabled.
	TosURL string `yaml:"tos_url,omitempty"`
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
	HeartbeatIntervalSeconds     int `yaml:"heartbeat_interval_seconds"`
	PinsPollIntervalSeconds      int `yaml:"pins_poll_interval_seconds"`
	MaxPinConcurrency            int `yaml:"max_pin_concurrency"`
	SuspectAfterMissedHeartbeats int `yaml:"suspect_after_missed_heartbeats"`
	UnreachableAfterSeconds      int `yaml:"unreachable_after_seconds"`
	EvictedAfterSeconds          int `yaml:"evicted_after_seconds"`
}

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
}

type WebhookDestination struct {
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"`
	Secret string   `yaml:"secret_file,omitempty"`
}

// Upload-pipeline defaults (M4). The size ceiling is an artificial Phase-1
// limit tied to V1 whole-object encryption; Phase-2 streaming AEAD lifts it.
const (
	DefaultMaxUploadSizeBytes    int64 = 104857600 // 100 MiB
	DefaultUploadSessionTTLSecs        = 86400      // 24h
	DefaultMaxConcurrentAssembly       = 8
)

// Uploads configures the M4 write path. Zero-valued fields are filled with the
// Default* constants by the loader (applyUploadDefaults).
type Uploads struct {
	MaxUploadSizeBytes    int64  `yaml:"max_upload_size_bytes"`
	SessionTTLSeconds     int    `yaml:"session_ttl_seconds"`
	MaxConcurrentAssembly int    `yaml:"max_concurrent_assembly"`
	TmpDir                string `yaml:"tmp_dir"`
}
