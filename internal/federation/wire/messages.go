// Package wire holds the federation protocol's shared, dependency-free types:
// the fed/v1 request/response messages, capability identifiers, normalized
// error codes, and the Ed25519 repair-token claim + verification. It is the
// only Nova package besides internal/secret that both the coordinator and the
// donor import. No operator-only dependencies may enter here.
package wire

// Protocol identifiers negotiated at register time.
const ProtocolV1 = "fed/v1"

// Capability identifiers (D-cap). A donor advertises the set it offers; the
// coordinator declares the set it requires.
const (
	CapPinChangeLog   = "pin-change-log/v1"
	CapSnapshot       = "snapshot/v1"
	CapBlobTransfer   = "blob-transfer/v1"  // M4: donor can fetch source-bearing assignments, verify, pin, ack
	CapRepairStream   = "repair-stream/v1"  // RESERVED for M5 donor-as-source; not advertised in M4
	CapReadSource     = "read-source/v1"   // M4.1: donor serves blobs to coordinator for donor-backed reads
	CapAuditBlockHash = "audit-block-hash/v1"
)

// CoordinatorSourceID is the reserved synthetic source identity for
// coordinator-as-source transfers (D-M4-2). It is a protocol constant shared by
// both sides — defined here in the dependency-free wire package so the donor
// (which cannot import the operator-only tokens package) can reference it as
// Ack.FetchedFromNodeID. It is NOT a nodes row. Fixed forever.
const CoordinatorSourceID = "00000000-0000-0000-0000-000000000001"

// Normalized machine-readable error codes carried in ErrorResponse.Code.
const (
	CodeSnapshotRequired  = "snapshot_required" // since_seq predates retention (D7)
	CodeIncompatible      = "incompatible_capabilities"
	CodeUnknownChangeKind = "unknown_change_kind" // fail-closed (D7)
	CodeStaleAssignment   = "stale_assignment"    // ack/fail for a superseded generation
)

// Fail.Reason domain (FEDERATION_PROTOCOL.md).
const (
	FailReasonOutOfSpace         = "out_of_space"
	FailReasonBlobUnavailable    = "blob_unavailable"
	FailReasonPolicyFilter       = "policy_filter"
	FailReasonNetworkError       = "network_error"
	FailReasonKuboError          = "kubo_error"
	FailReasonSourceUnauthorized = "source_unauthorized"
	FailReasonCIDMismatch        = "cid_mismatch"
	FailReasonBudgetExceeded     = "budget_exceeded"
	FailReasonOther              = "other"
)

// NormalizeFailReason maps "" to FailReasonOther and returns "" for an
// unrecognized reason (the /fail handler rejects that with 400).
func NormalizeFailReason(r string) string {
	switch r {
	case "":
		return FailReasonOther
	case FailReasonOutOfSpace, FailReasonBlobUnavailable, FailReasonPolicyFilter,
		FailReasonNetworkError, FailReasonKuboError, FailReasonSourceUnauthorized,
		FailReasonCIDMismatch, FailReasonBudgetExceeded, FailReasonOther:
		return r
	default:
		return ""
	}
}

// Change kinds. Donors fail closed on any other value (D7).
const (
	ChangeKindAssign = "assign"
	ChangeKindUnpin  = "unpin"
)

// ErrorResponse is the normalized error envelope for fed/v1 responses.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// RegisterRequest is sent by a donor; identity is derived from the verified
// mTLS cert, NOT these fields (D-cap). The fingerprints are echoed for
// cross-check against the verified peer cert; the rest are self-declared
// registration attributes plus the negotiation inputs.
type RegisterRequest struct {
	SupportedProtocols         []string       `json:"supported_protocols"`
	Capabilities               []string       `json:"capabilities"`
	ClientVersion              string         `json:"client_version,omitempty"`
	NebulaCertFingerprint      string         `json:"nebula_cert_fingerprint,omitempty"`
	FederationCertFingerprint  string         `json:"federation_cert_fingerprint,omitempty"`
	DisplayName                string         `json:"display_name,omitempty"`
	GeoDeclared                string         `json:"geo_declared,omitempty"`
	CapacityBytes              int64          `json:"capacity_bytes,omitempty"`
	BandwidthBudgetBytesPerDay int64          `json:"bandwidth_budget_bytes_per_day,omitempty"`
	PolicyFilters              map[string]any `json:"policy_filters,omitempty"`
	SourceNebulaAddr           string         `json:"source_nebula_addr,omitempty"` // M4.1: donor's read-source server address
}

// RegisterResponse confirms the selected protocol + required capabilities.
type RegisterResponse struct {
	SelectedProtocol     string   `json:"selected_protocol"`
	RequiredCapabilities []string `json:"required_capabilities"`
	NodeID               string   `json:"node_id"` // derived from the cert
}

// The remaining fed/v1 message shapes the M2–M4 handlers consume. Snapshot
// recovery (snapshot/epoch) gets its own types when M3 implements it.
type HeartbeatRequest struct {
	FreeBytes        int64  `json:"free_bytes"`
	StoredBytes      int64  `json:"stored_bytes"`
	SourceNebulaAddr string `json:"source_nebula_addr,omitempty"` // M4.1: donor's read-source server address
}

// ConfigUpdates carries operator-tunable federation timers back to a donor on
// each heartbeat so it can be retuned without redeploy.
type ConfigUpdates struct {
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds,omitempty"`
	PinsPollIntervalSeconds  int `json:"pins_poll_interval_seconds,omitempty"`
	MaxPinConcurrency        int `json:"max_pin_concurrency,omitempty"`
}

type HeartbeatResponse struct {
	ConfigUpdates        *ConfigUpdates `json:"config_updates"`
	CurrentEpoch         int64          `json:"current_epoch"`
	RepairTokenPublicKey string         `json:"repair_token_public_key,omitempty"` // empty until M4 (D1)
}
type ChangesRequest struct {
	SinceSeq int64 `json:"since_seq"`
}

// ChangeSource is the repair-fetch source for an assign change. Populated in M4
// (repair tokens); nil in M3.
type ChangeSource struct {
	NodeID     string `json:"node_id"`
	NebulaAddr string `json:"nebula_addr"`
	Token      string `json:"token"`
}

type PinChange struct {
	Sequence     int64         `json:"seq"`
	AssignmentID string        `json:"assignment_id"`
	Generation   int64         `json:"generation"`
	Kind         string        `json:"kind"`
	CID          string        `json:"cid"`
	ByteSize     int64         `json:"byte_size"`
	Source       *ChangeSource `json:"source,omitempty"` // M4
}

type ChangesResponse struct {
	Changes      []PinChange `json:"changes"`
	NextSeq      int64       `json:"next_seq"`
	CurrentEpoch int64       `json:"current_epoch"`
}

// SnapshotItem is one row of the recovery snapshot.
type SnapshotItem struct {
	CID          string `json:"cid"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	ByteSize     int64  `json:"byte_size"`
	AssignedAt   string `json:"assigned_at"` // RFC3339
}

type SnapshotResponse struct {
	Data          []SnapshotItem `json:"data"`
	Cursor        string         `json:"cursor"` // empty ⇒ last page
	SnapshotEpoch int64          `json:"snapshot_epoch"`
}

type Ack struct {
	AssignmentID      string `json:"assignment_id"`
	Generation        int64  `json:"generation"`
	CID               string `json:"cid"`
	ByteSize          int64  `json:"byte_size,omitempty"`
	IPFSPinStatus     string `json:"ipfs_pin_status,omitempty"`
	FetchedFromNodeID string `json:"fetched_from_node_id,omitempty"`
}

type Fail struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
	Reason       string `json:"reason"`
	Details      string `json:"details,omitempty"`
}
