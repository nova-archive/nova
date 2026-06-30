package config_test

import (
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/config"
)

func TestPossessionAuditDefaults(t *testing.T) {
	var p config.PossessionAudit // all zero values
	if p.EffectiveDeadline() != 30*time.Second {
		t.Fatal("deadline default")
	}
	if p.EffectiveAuditBudgetFraction() != 0.01 {
		t.Fatal("budget default")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("zero value must validate (means unset): %v", err)
	}
	if p.EffectiveBaseInterval() != 3600*time.Second {
		t.Fatal("base interval default")
	}
	if p.EffectiveMinAge() != 7*24*time.Hour {
		t.Fatal("min age default")
	}
}

func TestPossessionAuditValidationRejectsBadFraction(t *testing.T) {
	if err := (config.PossessionAudit{AuditBudgetFraction: 1.5}).Validate(); err == nil {
		t.Fatal("fraction > 1 must be rejected")
	}
	if err := (config.PossessionAudit{AuditBudgetFraction: -0.1}).Validate(); err == nil {
		t.Fatal("negative fraction must be rejected")
	}
	if err := (config.PossessionAudit{DeadlineSeconds: -1}).Validate(); err == nil {
		t.Fatal("negative deadline must be rejected")
	}
}
