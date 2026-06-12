//go:build novasim

package model

// Calibration holds per-operation costs measured from the REAL Nova
// primitives (internal/envelope, internal/ipfs) by the calib package. The
// model consumes these so throughput/availability ceilings are grounded in a
// real host rather than guessed. DefaultCalibration provides clearly-labelled
// fallback estimates so the model is runnable without a calibration pass; any
// reported number derived from defaults must say so.
type Calibration struct {
	Host     string `json:"host"`
	Cores    int    `json:"cores"`
	Measured bool   `json:"measured"` // false => DefaultCalibration estimates

	// AEAD throughput, bytes/sec on a single core.
	EncryptBytesPerSecPerCore float64 `json:"encrypt_bytes_per_sec_per_core"`
	DecryptBytesPerSecPerCore float64 `json:"decrypt_bytes_per_sec_per_core"`

	// Per-blob key unwrap (XChaCha20-Poly1305 of the 72-byte wrapped key),
	// seconds. This is the fixed per-read crypto overhead on top of decrypt.
	KeyUnwrapSeconds float64 `json:"key_unwrap_seconds"`

	// Deterministic IPFS import throughput (chunk + hash + blockstore write
	// + pin) for the dag-pb path, bytes/sec for a single import operation.
	ImportBytesPerSec float64 `json:"import_bytes_per_sec"`

	Notes string `json:"notes,omitempty"`
}

// DefaultCalibration returns conservative ESTIMATES (Measured=false). Real
// numbers come from `novasim calibrate`. XChaCha20-Poly1305 runs well over a
// GB/s/core on modern x86; deterministic IPFS import is dominated by chunking,
// hashing and blockstore writes and is far slower.
func DefaultCalibration(cores int) Calibration {
	return Calibration{
		Host:                      "estimated",
		Cores:                     cores,
		Measured:                  false,
		EncryptBytesPerSecPerCore: 1.4e9,
		DecryptBytesPerSecPerCore: 1.4e9,
		KeyUnwrapSeconds:          2e-6,
		ImportBytesPerSec:         1.5e8,
		Notes:                     "fallback estimates; run `novasim calibrate` for measured values",
	}
}
