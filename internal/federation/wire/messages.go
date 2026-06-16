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
	CapRepairStream   = "repair-stream/v1"
	CapAuditBlockHash = "audit-block-hash/v1"
)

// Normalized machine-readable error codes carried in ErrorResponse.Code.
const (
	CodeSnapshotRequired  = "snapshot_required" // since_seq predates retention (D7)
	CodeIncompatible      = "incompatible_capabilities"
	CodeUnknownChangeKind = "unknown_change_kind" // fail-closed (D7)
)

// ErrorResponse is the normalized error envelope for fed/v1 responses.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// RegisterRequest is sent by a donor; identity is derived from the verified
// mTLS cert, NOT these fields (D-cap). The JSON carries only negotiation inputs.
type RegisterRequest struct {
	SupportedProtocols []string `json:"supported_protocols"`
	Capabilities       []string `json:"capabilities"`
	ClientVersion      string   `json:"client_version,omitempty"`
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
	FreeBytes   int64 `json:"free_bytes"`
	StoredBytes int64 `json:"stored_bytes"`
}
type HeartbeatResponse struct {
	RepairTokenPublicKey string `json:"repair_token_public_key,omitempty"` // base64; delivered via config (D1)
}
type ChangesRequest struct {
	SinceSeq int64 `json:"since_seq"`
}
type ChangesResponse struct {
	Changes      []PinChange `json:"changes"`
	CurrentSeq   int64       `json:"current_seq"`
	CurrentEpoch int64       `json:"current_epoch"`
}
type PinChange struct {
	Sequence     int64  `json:"sequence"`
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	Kind         string `json:"kind"`
	CID          string `json:"cid"`
}
type Ack struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
}
type Fail struct {
	AssignmentID string `json:"assignment_id"`
	Generation   int64  `json:"generation"`
	CID          string `json:"cid"`
	Reason       string `json:"reason"`
}
