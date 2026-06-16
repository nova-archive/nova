package config

import "testing"

func TestFederationValidateLoopbackSkipsInterfaceGuard(t *testing.T) {
	f := Federation{
		ListenAddr:       "127.0.0.1:9443",
		NebulaInterface:  "nebula1",
		FederationCAPath: "x", FederationCertPath: "y", FederationKeyPath: "z",
	}
	if err := f.Validate(true /* dev */); err != nil {
		t.Fatalf("loopback dev should skip interface guard: %v", err)
	}
}

func TestFederationValidateRequiresMaterialWhenEnabled(t *testing.T) {
	f := Federation{ListenAddr: "10.42.0.1:9443"}
	if err := f.Validate(false); err == nil {
		t.Fatal("expected error: missing cert paths")
	}
}

func TestFederationValidateDisabledWhenNoListen(t *testing.T) {
	if err := (Federation{}).Validate(false); err != nil {
		t.Fatalf("empty federation (disabled) must validate: %v", err)
	}
}

func TestFederationTimersDefaults(t *testing.T) {
	hb, poll, conc := (Federation{}).FederationTimers()
	if hb != DefaultHeartbeatIntervalSeconds || poll != DefaultPinsPollIntervalSeconds || conc != DefaultMaxPinConcurrency {
		t.Fatalf("defaults = %d/%d/%d", hb, poll, conc)
	}
}
