// Package envelope owns Nova's encryption envelope wire format.
//
// The v1 wire format is specified in docs/specs/ENCRYPTION_ENVELOPE.md.
// Implementations of the Codec interface MUST be byte-for-byte
// compatible with the spec; integrity audits will catch drift.
//
// The package is deliberately DB-naïve: only keystore.go imports the
// pgx pool. The pure-crypto pieces (v1.go, keywrap.go) accept byte
// slices and master-key bytes directly so they can be exercised in
// fast unit tests and re-used wherever needed (e.g., novactl tooling).
package envelope

import "errors"

// Sentinel errors returned by Decode and the Codec.Decrypt path.
// Callers compare with errors.Is to map to HTTP statuses or audit
// failures.
var (
	// ErrEnvelopeTooShort: byte slice is shorter than the 32-byte
	// header. Maps to a 400 Bad Request at the API layer; in the
	// integrity audit this surfaces as envelope_decode = fail.
	ErrEnvelopeTooShort = errors.New("envelope: too short")

	// ErrEnvelopeBadMagic: header does not start with ASCII "NOVE".
	ErrEnvelopeBadMagic = errors.New("envelope: bad magic")

	// ErrEnvelopeUnsupported: header parses but the version, algorithm,
	// or reserved bytes are not recognized by this implementation.
	// Distinct from BadMagic to support future migrations.
	ErrEnvelopeUnsupported = errors.New("envelope: unsupported version or algorithm")

	// ErrEnvelopeAuthFailed: AEAD authentication tag failed. Always
	// treat as adversarial: do not retry, do not leak detail.
	ErrEnvelopeAuthFailed = errors.New("envelope: authentication failed")

	// ErrKeyWrongLength: per-blob or master key is not 32 bytes.
	ErrKeyWrongLength = errors.New("envelope: key wrong length")

	// ErrWrappedKeyWrongLength: wrapped key blob is not 72 bytes.
	ErrWrappedKeyWrongLength = errors.New("envelope: wrapped key wrong length")
)
