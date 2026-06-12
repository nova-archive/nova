//go:build novasim

package model

// The coordinator-bottleneck model. Nova routes EVERY read through the single
// coordinator, which decrypts on the fly (T1.26: donor-blind, not
// operator-blind). Donor durability scales with the fleet, but read egress and
// upload ingest are capped by one machine's NIC, CPU and Postgres. This is the
// real availability single-point-of-failure — distinct from durability — and
// the thing multi-coordinator HA (proposed Phase 6) addresses.

// CoordinatorSpec describes one coordinator host's serving resources.
type CoordinatorSpec struct {
	Cores          int     // CPU cores available to the read/decrypt path
	NICBytesPerSec float64 // network uplink, bytes/sec
	DBOpsPerSec    float64 // Postgres metadata-lookup ceiling (blob row + key row)
	FixedCPUPerReq float64 // non-crypto per-request CPU overhead, seconds
}

// DefaultCoordinatorSpec returns a modest production coordinator: 8 cores,
// 1 Gbps uplink, a pooled-Postgres read ceiling, and a small per-request CPU
// overhead. These are deliberately conservative; sweep them in the CLI.
func DefaultCoordinatorSpec() CoordinatorSpec {
	return CoordinatorSpec{
		Cores:          8,
		NICBytesPerSec: 1e9 / 8.0, // 1 Gbps
		DBOpsPerSec:    40000,     // ~16-conn pool, matches key_rotation_load.py
		FixedCPUPerReq: 50e-6,     // 50 µs routing/auth/serialisation
	}
}

// ReadCeiling is the saturation analysis for the read path at a given mean
// object size. All three limbs are reported so the binding constraint is
// explicit.
type ReadCeiling struct {
	MeanObjectBytes     float64
	CPUBoundBytesPerSec float64
	CPUBoundQPS         float64
	NICBoundBytesPerSec float64
	DBBoundBytesPerSec  float64
	DBBoundQPS          float64

	EgressCeilingBytesPerSec float64 // min of the three limbs
	QPSCeiling               float64
	Binding                  string // "cpu" | "nic" | "db"
}

// ReadCeilingFor computes the single-coordinator read ceiling for a mean
// object size, given a calibration and a host spec.
func ReadCeilingFor(cal Calibration, spec CoordinatorSpec, meanObjectBytes float64) ReadCeiling {
	// CPU-seconds per read = key unwrap + fixed overhead + decrypt of the object.
	cpuPerReq := cal.KeyUnwrapSeconds + spec.FixedCPUPerReq + meanObjectBytes/cal.DecryptBytesPerSecPerCore
	cpuQPS := float64(spec.Cores) / cpuPerReq
	cpuBytes := cpuQPS * meanObjectBytes

	nicBytes := spec.NICBytesPerSec

	dbQPS := spec.DBOpsPerSec
	dbBytes := dbQPS * meanObjectBytes

	rc := ReadCeiling{
		MeanObjectBytes:     meanObjectBytes,
		CPUBoundBytesPerSec: cpuBytes,
		CPUBoundQPS:         cpuQPS,
		NICBoundBytesPerSec: nicBytes,
		DBBoundBytesPerSec:  dbBytes,
		DBBoundQPS:          dbQPS,
	}
	rc.EgressCeilingBytesPerSec = cpuBytes
	rc.Binding = "cpu"
	if nicBytes < rc.EgressCeilingBytesPerSec {
		rc.EgressCeilingBytesPerSec = nicBytes
		rc.Binding = "nic"
	}
	if dbBytes < rc.EgressCeilingBytesPerSec {
		rc.EgressCeilingBytesPerSec = dbBytes
		rc.Binding = "db"
	}
	rc.QPSCeiling = rc.EgressCeilingBytesPerSec / meanObjectBytes
	return rc
}

// UploadCeiling is the single-coordinator ingest analysis. Uploads encrypt and
// deterministically import; import dominates.
type UploadCeiling struct {
	MeanObjectBytes          float64
	CPUBoundBytesPerSec      float64 // encrypt + import across cores
	NICBoundBytesPerSec      float64 // ingress
	IngestCeilingBytesPerSec float64
	ConcurrentUploadsAt      float64 // sustainable in-flight uploads at the ceiling
	Binding                  string
}

// UploadCeilingFor computes the single-coordinator upload-ingest ceiling.
// Per upload, CPU-seconds = encrypt(size) + import(size); import is modelled as
// a throughput (bytes/sec) since it is I/O + hashing rather than pure CPU.
func UploadCeilingFor(cal Calibration, spec CoordinatorSpec, meanObjectBytes float64) UploadCeiling {
	encryptSec := meanObjectBytes / cal.EncryptBytesPerSecPerCore
	importSec := meanObjectBytes / cal.ImportBytesPerSec
	perUploadSec := encryptSec + importSec
	// Across cores: ingest bytes/sec = cores * size / perUploadSec.
	cpuBytes := float64(spec.Cores) * meanObjectBytes / perUploadSec
	nicBytes := spec.NICBytesPerSec

	uc := UploadCeiling{MeanObjectBytes: meanObjectBytes, CPUBoundBytesPerSec: cpuBytes, NICBoundBytesPerSec: nicBytes}
	uc.IngestCeilingBytesPerSec = cpuBytes
	uc.Binding = "cpu(import)"
	if nicBytes < uc.IngestCeilingBytesPerSec {
		uc.IngestCeilingBytesPerSec = nicBytes
		uc.Binding = "nic"
	}
	// Little's law-ish: sustainable in-flight uploads = throughput × latency.
	uc.ConcurrentUploadsAt = (uc.IngestCeilingBytesPerSec / meanObjectBytes) * perUploadSec
	return uc
}

// PerDay converts a bytes/sec rate to bytes/day for human-scale reporting.
func PerDay(bytesPerSec float64) float64 { return bytesPerSec * 86400 }
