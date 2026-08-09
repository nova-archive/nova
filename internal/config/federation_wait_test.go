package config_test

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/config"
)

func TestWaitForInterfaceAddr_LoopbackSucceedsImmediately(t *testing.T) {
	if err := config.WaitForInterfaceAddr(context.Background(), "lo", "127.0.0.1:9443", 2*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("want nil for loopback, got %v", err)
	}
}

func TestWaitForInterfaceAddr_TimesOutOnAbsentInterface(t *testing.T) {
	start := time.Now()
	err := config.WaitForInterfaceAddr(context.Background(), "nova-no-such-iface", "10.42.0.1:9443", 150*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("returned too early (%v) — it must poll until the deadline", elapsed)
	}
}

func TestWaitForInterfaceAddr_ContextCancelWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := config.WaitForInterfaceAddr(ctx, "nova-no-such-iface", "10.42.0.1:9443", time.Minute, time.Millisecond); err == nil {
		t.Fatal("want error on cancelled context")
	}
}

// TestWaitForInterfaceAddr_ZeroTimeoutChecksOnce proves interface_wait_seconds: -1
// (which resolves to a zero timeout) keeps the pre-M7.2 fail-fast behaviour.
func TestWaitForInterfaceAddr_ZeroTimeoutChecksOnce(t *testing.T) {
	start := time.Now()
	if err := config.WaitForInterfaceAddr(context.Background(), "nova-no-such-iface", "10.42.0.1:9443", 0, time.Second); err == nil {
		t.Fatal("want immediate error with a zero timeout")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("zero timeout must not sleep, took %v", elapsed)
	}
}

func TestInterfaceWaitTimeout_Defaults(t *testing.T) {
	if got := (config.Federation{}).InterfaceWaitTimeout(); got != config.DefaultInterfaceWaitSeconds*time.Second {
		t.Fatalf("unset = %v, want the %ds default", got, config.DefaultInterfaceWaitSeconds)
	}
	if got := (config.Federation{InterfaceWaitSeconds: 30}).InterfaceWaitTimeout(); got != 30*time.Second {
		t.Fatalf("explicit = %v, want 30s", got)
	}
	if got := (config.Federation{InterfaceWaitSeconds: -1}).InterfaceWaitTimeout(); got != 0 {
		t.Fatalf("-1 = %v, want 0 (fail fast)", got)
	}
}
