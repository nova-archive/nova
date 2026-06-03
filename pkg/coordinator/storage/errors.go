// Package storage is the coordinator's read core. It resolves a blob's
// state and visibility from Postgres, fetches the envelope from the IPFS
// backend, unwraps the per-blob key via the keystore, and decrypts.
//
// The package is HTTP-naïve: it returns the sentinel errors below and the
// internal/api layer maps them to status codes. The same ErrBlobAuthRequired
// maps to 401 on the bytes route and 404 on the .json route.
package storage

import "errors"

var (
	// ErrBlobNotFound: no blobs row for the CID, or the CID is malformed.
	ErrBlobNotFound = errors.New("storage: blob not found")

	// ErrBlobAuthRequired: blob is private (no public/unlisted collection
	// membership). Recoverable via signed URL / bearer in M7 / M6.
	ErrBlobAuthRequired = errors.New("storage: authorization required")

	// ErrBlobQuarantined: blob is under moderation hold.
	ErrBlobQuarantined = errors.New("storage: blob quarantined")

	// ErrBlobBlocklisted: the CID is on the operator-curated blocklist (M9).
	// Denied on both the read path and at import/commit. Effective for
	// public_archival (deterministic CID); a permanent exact-CID deny otherwise.
	ErrBlobBlocklisted = errors.New("storage: blob blocklisted")

	// ErrBlobSoftDeleted: blob soft-deleted (bytes may still exist).
	ErrBlobSoftDeleted = errors.New("storage: blob soft-deleted")

	// ErrBlobTombstoned: blob tombstoned (key shredded).
	ErrBlobTombstoned = errors.New("storage: blob tombstoned")

	// ErrKeyShredded: encrypted blob whose DEK has been crypto-shredded.
	ErrKeyShredded = errors.New("storage: encryption key shredded")
)

// Write-path domain errors (M4). The internal/api layer maps these to status
// codes (413 / 400 / 404 / 503 respectively).
var (
	// ErrUploadTooLarge: declared size exceeds the configured ceiling.
	ErrUploadTooLarge = errors.New("storage: upload exceeds max size")

	// ErrMimeRejected: the declared MIME contradicts the sniffed content.
	ErrMimeRejected = errors.New("storage: declared mime contradicts content")

	// ErrCollectionNotFound: the upload targets a nonexistent collection.
	ErrCollectionNotFound = errors.New("storage: collection not found")

	// ErrServerBusy: the in-memory assembly semaphore is saturated.
	ErrServerBusy = errors.New("storage: assembly capacity saturated")

	// ErrModerationRejected: the upload was rejected by the product's moderation hook.
	ErrModerationRejected = errors.New("storage: upload rejected by moderation")
)
