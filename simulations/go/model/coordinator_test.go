//go:build novasim

package model

import (
	"math"
	"testing"
)

func TestReadCeilingNICBoundForMediaWorkload(t *testing.T) {
	cal := DefaultCalibration(8)
	spec := DefaultCoordinatorSpec() // 1 Gbps NIC
	rc := ReadCeilingFor(cal, spec, 0.5*MiB)

	// A 1 Gbps coordinator serving 0.5 MiB objects is network-bound: fast
	// cores and a big donor fleet cannot lift egress past the single uplink.
	if rc.Binding != "nic" {
		t.Errorf("binding = %q, want nic (egress=%s cpu=%s db=%s)", rc.Binding,
			humanBytes(rc.EgressCeilingBytesPerSec), humanBytes(rc.CPUBoundBytesPerSec), humanBytes(rc.DBBoundBytesPerSec))
	}
	if math.Abs(rc.EgressCeilingBytesPerSec-spec.NICBytesPerSec) > 1 {
		t.Errorf("egress ceiling %s, want NIC %s", humanBytes(rc.EgressCeilingBytesPerSec), humanBytes(spec.NICBytesPerSec))
	}
	t.Logf("1 Gbps coordinator, 0.5 MiB objects: %.0f req/s, %s/day egress (binding=%s)",
		rc.QPSCeiling, humanBytes(PerDay(rc.EgressCeilingBytesPerSec)), rc.Binding)
}

func TestReadCeilingCPUBindsWithoutNetworkLimit(t *testing.T) {
	cal := DefaultCalibration(1)
	spec := DefaultCoordinatorSpec()
	spec.Cores = 1
	spec.NICBytesPerSec = 100e9 / 8.0 // 100 GbE takes the network out of the picture
	rc := ReadCeilingFor(cal, spec, 8*MiB)
	if rc.Binding != "cpu" {
		t.Errorf("binding = %q, want cpu with 1 decrypt core and no network limit", rc.Binding)
	}
	t.Logf("1 decrypt core (network-unlimited), 8 MiB objects: %s/s egress (binding=%s) — a single core caps decrypt throughput",
		humanBytes(rc.EgressCeilingBytesPerSec), rc.Binding)
}

func TestUploadCeilingImportBound(t *testing.T) {
	cal := DefaultCalibration(8)
	spec := DefaultCoordinatorSpec()
	spec.NICBytesPerSec = 10e9 / 8.0 // take NIC out of the picture
	uc := UploadCeilingFor(cal, spec, 4*MiB)
	if uc.Binding != "cpu(import)" {
		t.Errorf("binding = %q, want cpu(import)", uc.Binding)
	}
	if uc.IngestCeilingBytesPerSec >= float64(spec.Cores)*cal.EncryptBytesPerSecPerCore {
		t.Errorf("ingest ceiling should be import-limited, well below encrypt throughput")
	}
	t.Logf("8-core coordinator upload ingest (4 MiB objects): %s/s, ~%.0f concurrent",
		humanBytes(uc.IngestCeilingBytesPerSec), uc.ConcurrentUploadsAt)
}
