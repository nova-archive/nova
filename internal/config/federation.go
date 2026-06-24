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

// RepairTokenTTL returns the repair-token validity window (default 300s, clamped
// to the spec range 60..1800).
func (f Federation) RepairTokenTTL() time.Duration {
	s := f.RepairTokenTTLSeconds
	if s == 0 {
		s = 300
	}
	if s < 60 {
		s = 60
	}
	if s > 1800 {
		s = 1800
	}
	return time.Duration(s) * time.Second
}

// MaxTransfer is the hard cap on a single coordinator-as-source transfer (default
// 100 MiB).
func (f Federation) MaxTransfer() int64 {
	if f.MaxTransferBytes > 0 {
		return f.MaxTransferBytes
	}
	return 100 * 1024 * 1024
}

// SourceStaleSeconds is the donor-freshness window the coordinator's donor-fetch
// tier (P2-M4.1) passes to ListSourceableHolders: a holder whose last_seen_at is
// older than this is not considered sourceable. It is derived from the heartbeat
// interval and the suspect threshold (a node missing this many heartbeats is
// already suspect), with a small grace multiplier, clamped to a sane floor of
// 10 minutes so a brief poll gap never empties the candidate set.
func (f Federation) SourceStaleSeconds() float64 {
	hb := f.HeartbeatIntervalSeconds
	if hb <= 0 {
		hb = DefaultHeartbeatIntervalSeconds
	}
	misses := f.SuspectAfterMissedHeartbeats
	if misses <= 0 {
		misses = 3
	}
	// One extra interval of grace beyond the suspect threshold.
	secs := float64(hb) * float64(misses+1)
	if secs < 600 {
		secs = 600
	}
	return secs
}

// Default donor read-path containment knobs (P2-M4.1 Task 8). These mirror the
// defaults baked into storage.ReadSourceConfig.withDefaults; both must stay in
// sync. The donor-backed read tier treats each fetch as a protected integration
// point: a per-fetch timeout, a coordinator-wide bulkhead, a per-donor
// concurrency cap, a per-donor circuit breaker, and bounded fallback.
const (
	DefaultReadSourceTimeoutSeconds      = 30
	DefaultReadSourceBulkhead            = 16
	DefaultReadSourcePerDonorLimit       = 4
	DefaultReadSourceBreakerThreshold    = 5
	DefaultReadSourceBreakerCooldownSecs = 30
	DefaultReadSourceMaxFallbacks        = 3
)

// ReadSourceTimeout is the per-holder donor fetch+read timeout (default 30s).
func (f Federation) ReadSourceTimeout() time.Duration {
	s := f.ReadSourceTimeoutSeconds
	if s <= 0 {
		s = DefaultReadSourceTimeoutSeconds
	}
	return time.Duration(s) * time.Second
}

// ReadSourceBulkheadLimit is the coordinator-wide max concurrent donor fetches
// (default 16).
func (f Federation) ReadSourceBulkheadLimit() int {
	if f.ReadSourceBulkhead <= 0 {
		return DefaultReadSourceBulkhead
	}
	return f.ReadSourceBulkhead
}

// ReadSourcePerDonorFetchLimit is the max concurrent fetches to a single donor
// (default 4).
func (f Federation) ReadSourcePerDonorFetchLimit() int {
	if f.ReadSourcePerDonorLimit <= 0 {
		return DefaultReadSourcePerDonorLimit
	}
	return f.ReadSourcePerDonorLimit
}

// ReadSourceBreakerThresholdK is the consecutive-failure count that opens a
// per-donor breaker (default 5).
func (f Federation) ReadSourceBreakerThresholdK() int {
	if f.ReadSourceBreakerThreshold <= 0 {
		return DefaultReadSourceBreakerThreshold
	}
	return f.ReadSourceBreakerThreshold
}

// ReadSourceBreakerCooldown is the half-open delay after a breaker opens
// (default 30s).
func (f Federation) ReadSourceBreakerCooldown() time.Duration {
	s := f.ReadSourceBreakerCooldownSecs
	if s <= 0 {
		s = DefaultReadSourceBreakerCooldownSecs
	}
	return time.Duration(s) * time.Second
}

// ReadSourceMaxFallbacksPerRequest is the max donor fetch ATTEMPTS per request
// (default 3). A breaker SKIP does not consume a fallback.
func (f Federation) ReadSourceMaxFallbacksPerRequest() int {
	if f.ReadSourceMaxFallbacks <= 0 {
		return DefaultReadSourceMaxFallbacks
	}
	return f.ReadSourceMaxFallbacks
}
