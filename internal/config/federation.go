package config

import (
	"fmt"
	"net"
	"time"
)

// Default federation timers (mirrors FEDERATION_PROTOCOL.md).
const (
	DefaultHeartbeatIntervalSeconds = 300
	DefaultPinsPollIntervalSeconds  = 600
	DefaultMaxPinConcurrency        = 16
	DefaultChangeLogRetentionHours  = 168 // 7 days
)

// Validate checks the federation block. dev=true (loopback/test) skips the
// interface-membership guard. A disabled block (no listen_addr) is always valid.
func (f Federation) Validate(dev bool) error {
	if !f.Enabled() {
		return nil
	}
	if _, _, err := net.SplitHostPort(f.ListenAddr); err != nil {
		return fmt.Errorf("federation.listen_addr %q is not host:port: %w", f.ListenAddr, err)
	}
	for name, p := range map[string]string{
		"federation_ca_path":   f.FederationCAPath,
		"federation_cert_path": f.FederationCertPath,
		"federation_key_path":  f.FederationKeyPath,
	} {
		if p == "" {
			return fmt.Errorf("federation.%s is required when listen_addr is set", name)
		}
	}
	if f.NebulaInterface != "" && !dev {
		if err := f.checkListenOnInterface(); err != nil {
			return err
		}
	}
	return nil
}

// checkListenOnInterface verifies the listen IP belongs to nebula_interface —
// catching the accidental 0.0.0.0/public-interface foot-gun at boot.
func (f Federation) checkListenOnInterface() error {
	host, _, _ := net.SplitHostPort(f.ListenAddr)
	ifi, err := net.InterfaceByName(f.NebulaInterface)
	if err != nil {
		return fmt.Errorf("federation.nebula_interface %q: %w", f.NebulaInterface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return err
	}
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip != nil && ip.String() == host {
			return nil
		}
	}
	return fmt.Errorf("federation.listen_addr host %q is not an address of interface %q", host, f.NebulaInterface)
}

// FederationTimers fills defaults and returns the timer triple delivered to
// donors via heartbeat config_updates.
func (f Federation) FederationTimers() (heartbeat, poll, concurrency int) {
	heartbeat, poll, concurrency = f.HeartbeatIntervalSeconds, f.PinsPollIntervalSeconds, f.MaxPinConcurrency
	if heartbeat == 0 {
		heartbeat = DefaultHeartbeatIntervalSeconds
	}
	if poll == 0 {
		poll = DefaultPinsPollIntervalSeconds
	}
	if concurrency == 0 {
		concurrency = DefaultMaxPinConcurrency
	}
	return heartbeat, poll, concurrency
}

// FederationRetention returns the change-log retention window and prune cadence.
// Retention defaults to DefaultChangeLogRetentionHours; the prune poll is 1h.
func (f Federation) FederationRetention() (retention, prunePoll time.Duration) {
	hours := f.ChangeLogRetentionHours
	if hours <= 0 {
		hours = DefaultChangeLogRetentionHours
	}
	return time.Duration(hours) * time.Hour, time.Hour
}
