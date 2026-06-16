package wire

import (
	"encoding/json"
	"testing"
)

func TestRegisterRequestRoundTrip(t *testing.T) {
	in := RegisterRequest{
		SupportedProtocols:         []string{ProtocolV1},
		Capabilities:               []string{},
		ClientVersion:              "0.2.0",
		NebulaCertFingerprint:      "sha256:nebula",
		FederationCertFingerprint:  "sha256:fed",
		DisplayName:                "donor-a",
		GeoDeclared:                "DE",
		CapacityBytes:              1 << 40,
		BandwidthBudgetBytesPerDay: 1 << 35,
		PolicyFilters:              map[string]any{"max_blob_bytes": float64(1048576)},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RegisterRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.DisplayName != "donor-a" || out.CapacityBytes != 1<<40 || out.FederationCertFingerprint != "sha256:fed" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

func TestHeartbeatResponseShape(t *testing.T) {
	r := HeartbeatResponse{
		ConfigUpdates: &ConfigUpdates{HeartbeatIntervalSeconds: 300},
		CurrentEpoch:  0,
	}
	b, _ := json.Marshal(r)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"config_updates", "current_epoch"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %s", k, b)
		}
	}
}
