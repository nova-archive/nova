package config_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/config"
)

// P2-M7.1 (D-M7.1-3): the below_floor_replacement block follows the M6
// possession_audit pattern — zero values mean "unset" and default via the
// Effective* accessors; Validate rejects only invalid explicit values.

func TestBelowFloorReplacementDefaults(t *testing.T) {
	var b config.BelowFloorReplacement // all zero values
	if !b.EffectiveEnabled() {
		t.Fatal("enabled must default to true (the sweep is on unless the operator opts out)")
	}
	if b.EffectiveHysteresisMargin() != 0.05 {
		t.Fatal("hysteresis_margin default")
	}
	if b.EffectiveGrace() != 24*time.Hour {
		t.Fatal("grace default (24h)")
	}
	if b.EffectiveRequeueBatch() != 500 {
		t.Fatal("requeue_batch default")
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("zero value must validate (means unset): %v", err)
	}
}

func TestBelowFloorReplacementExplicitValues(t *testing.T) {
	off := false
	b := config.BelowFloorReplacement{
		Enabled:          &off,
		HysteresisMargin: 0.1,
		GraceSeconds:     3600,
		RequeueBatch:     50,
	}
	if b.EffectiveEnabled() {
		t.Fatal("explicit enabled=false must win over the default")
	}
	if b.EffectiveHysteresisMargin() != 0.1 {
		t.Fatal("explicit margin")
	}
	if b.EffectiveGrace() != time.Hour {
		t.Fatal("explicit grace")
	}
	if b.EffectiveRequeueBatch() != 50 {
		t.Fatal("explicit batch")
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("explicit valid values must validate: %v", err)
	}
}

func TestBelowFloorReplacementValidationRejectsBadValues(t *testing.T) {
	if err := (config.BelowFloorReplacement{HysteresisMargin: 1.5}).Validate(); err == nil {
		t.Fatal("margin > 1 must be rejected")
	}
	if err := (config.BelowFloorReplacement{HysteresisMargin: -0.1}).Validate(); err == nil {
		t.Fatal("negative margin must be rejected")
	}
	if err := (config.BelowFloorReplacement{GraceSeconds: -1}).Validate(); err == nil {
		t.Fatal("negative grace must be rejected")
	}
	if err := (config.BelowFloorReplacement{RequeueBatch: -1}).Validate(); err == nil {
		t.Fatal("negative batch must be rejected")
	}
}
