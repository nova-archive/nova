package ipfs

import (
	"context"
	"io"

	"github.com/ipfs/go-cid"
)

// Mode determines which set of hardening rules ValidateConfig enforces.
// Private (default) refuses public-DHT-shaped configs entirely;
// PublicArchivalDHT relaxes Routing/Provider/Bootstrap rules per the
// opt-in mode in docs/specs/KUBO_HARDENING.md § "Public IPFS DHT mode".
type Mode int

const (
	// ModePrivate is the default — every Phase 1 deployment that holds
	// personal or potentially-infringing content uses this.
	ModePrivate Mode = iota

	// ModePublicArchivalDHT is the opt-in for `nova-archive`-style
	// deployments hosting open data. Operator must explicitly set
	// `coordinator.public_ipfs_dht: true` in operator.yaml.
	ModePublicArchivalDHT
)

// String returns the mode name for log messages.
func (m Mode) String() string {
	switch m {
	case ModePrivate:
		return "private"
	case ModePublicArchivalDHT:
		return "public_archival_dht"
	default:
		return "unknown"
	}
}

// AddResult is what AddDeterministic returns. The Blocks slice is in
// DAG-traversal order matching the blob_blocks.block_index sequence in
// the database.
type AddResult struct {
	CID          cid.Cid
	EnvelopeSize int64
	Codec        string // "raw" for single-block, "dag-pb" for UnixFS
	Blocks       []Block
	MerkleRoot   cid.Cid // root CID; for single-block this equals CID
}

// Block is one row's worth of blob_blocks information.
type Block struct {
	CID   cid.Cid
	Index int
	Size  int
}

// Backend is Nova's IPFS abstraction. EmbeddedBackend implements it via
// in-process Kubo; future Phase 2 work may add a remote backend that
// talks to an external Kubo daemon over the loopback HTTP API.
type Backend interface {
	// AddDeterministic imports the bytes per IPFS_IMPORT_RULES.md. The
	// returned CID is bit-identical across implementations conforming
	// to the spec. The bytes are pinned (the implementation MAY use the
	// raw-codec shortcut path for bytes ≤ RawCodecThresholdBytes).
	AddDeterministic(ctx context.Context, envelope []byte) (AddResult, error)

	// Get retrieves the previously-Add'd bytes for a CID. The returned
	// ReadCloser MUST be closed by the caller.
	Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error)

	// Has reports whether the local blockstore has the CID pinned.
	Has(ctx context.Context, c cid.Cid) (bool, error)

	// Pin pins an already-stored CID (useful when re-pinning after an
	// audit detects a missing pin).
	Pin(ctx context.Context, c cid.Cid) error

	// Unpin removes the local pin so Kubo's GC can reclaim. Does not
	// remove the blocks immediately.
	Unpin(ctx context.Context, c cid.Cid) error

	// BlockstoreHas reports whether a specific block CID exists in the
	// blockstore. Used by the block_hash_valid integrity audit.
	BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error)

	// BlockGet returns the raw block bytes for a CID, bypassing UnixFS
	// reassembly. Used by the block_hash_valid integrity audit.
	BlockGet(ctx context.Context, c cid.Cid) ([]byte, error)

	// Close releases the backend's resources. After Close, all methods
	// return errors.
	Close(ctx context.Context) error

	// Health is a lightweight liveness probe. Returns nil when the backend
	// is operational, an error when it is not. Intended for /readyz; must
	// be cheap (no I/O beyond an in-process state check or a tiny local
	// metadata read) so /readyz can be polled frequently.
	Health(ctx context.Context) error
}
