// Package ipfs is Nova's IPFS abstraction layer. The Backend interface
// defines the operations the coordinator and product layers perform
// against an IPFS daemon; the EmbeddedBackend implementation runs an
// in-process Kubo node configured per docs/specs/KUBO_HARDENING.md and
// docs/specs/IPFS_IMPORT_RULES.md.
//
// The interface is small on purpose: most call sites only need
// Add/Get/Has, and the audit subsystem additionally needs
// BlockstoreHas/BlockGet. Splitting these into multiple interfaces was
// considered and rejected — there is exactly one implementation in
// Phase 1, and the surface is stable enough that future product layers
// can mock it via testify/mock.
package ipfs

import "errors"

var (
	// ErrCIDNotPinned: blob is not present in the local Kubo blockstore
	// (Phase 1 single-node) or has been unpinned. Surfaces at the
	// integrity audit kubo_pin_present check.
	ErrCIDNotPinned = errors.New("ipfs: CID not pinned locally")

	// ErrBlockNotFound: a specific block CID referenced by blob_blocks
	// is missing. Surfaces at the integrity audit block_hash_valid
	// check.
	ErrBlockNotFound = errors.New("ipfs: block not found")

	// ErrConfigViolation: a KUBO_HARDENING.md rule was violated during
	// ValidateConfig. The wrapped error names the specific key.
	ErrConfigViolation = errors.New("ipfs: kubo config violates hardening rule")

	// ErrSwarmKeyMissing: private-mode requires IPFS_SWARM_KEY at the
	// repo path; Backend.Run refuses to start without it.
	ErrSwarmKeyMissing = errors.New("ipfs: swarm key missing (required in private mode)")
)
