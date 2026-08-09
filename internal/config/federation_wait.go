package config

import (
	"context"
	"fmt"
	"time"
)

// DefaultInterfaceWaitSeconds bounds the boot wait for nebula_interface to
// carry listen_addr.
const DefaultInterfaceWaitSeconds = 120

// WaitForInterfaceAddr polls until hostPort's host is an address of iface, the
// timeout elapses, or ctx is done.
//
// It exists because the federation listener must not fail the process when the
// Nebula sidecar has not yet created the overlay interface (P2-M7.2
// D-M7.2-8b). The sidecar runs with network_mode "service:coordinator", so it
// cannot create nebula1 until this process is already running — a coordinator
// that exits on a missing interface flaps the sidecar's attach target forever.
//
// A zero timeout checks exactly once and returns, preserving the pre-M7.2
// fail-fast behaviour for operators who configure interface_wait_seconds: -1.
func WaitForInterfaceAddr(ctx context.Context, iface, hostPort string, timeout, poll time.Duration) error {
	f := Federation{ListenAddr: hostPort, NebulaInterface: iface}
	deadline := time.Now().Add(timeout)
	for {
		last := f.checkListenOnInterface()
		if last == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s on %s: %w", hostPort, iface, err)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s on %s: %w", timeout, hostPort, iface, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s on %s: %w", hostPort, iface, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// InterfaceWaitTimeout is how long boot waits for nebula_interface to carry
// listen_addr. Unset uses DefaultInterfaceWaitSeconds; a negative value means
// no wait at all (fail fast, the pre-M7.2 behaviour).
func (f Federation) InterfaceWaitTimeout() time.Duration {
	if f.InterfaceWaitSeconds < 0 {
		return 0
	}
	if f.InterfaceWaitSeconds == 0 {
		return DefaultInterfaceWaitSeconds * time.Second
	}
	return time.Duration(f.InterfaceWaitSeconds) * time.Second
}
