package ipfs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// KuboConfig mirrors the fields of Kubo's on-disk config.json that the
// hardening validator inspects. We intentionally do not import Kubo's
// config struct here — the validator is a pure-Go check we can run
// before any Kubo code loads, and decoupling from Kubo's internals
// limits the blast radius of Kubo version upgrades.
type KuboConfig struct {
	Bootstrap  []string         `json:"Bootstrap"`
	Routing    RoutingConfig    `json:"Routing"`
	Discovery  DiscoveryConfig  `json:"Discovery"`
	Provider   ProviderConfig   `json:"Provider"`
	Reprovider ReproviderConfig `json:"Reprovider"`
	Addresses  AddressesConfig  `json:"Addresses"`
	Swarm      SwarmConfig      `json:"Swarm"`
}

type RoutingConfig struct {
	Type string `json:"Type"`
}

type DiscoveryConfig struct {
	MDNS MDNSConfig `json:"MDNS"`
}

type MDNSConfig struct {
	Enabled bool `json:"Enabled"`
}

type ProviderConfig struct {
	Strategy string `json:"Strategy"`
}

type ReproviderConfig struct {
	Strategy string `json:"Strategy"`
}

type AddressesConfig struct {
	API     string `json:"API"`
	Gateway string `json:"Gateway"`
}

type SwarmConfig struct {
	DisableNatPortMap bool `json:"DisableNatPortMap"`
}

// ValidateConfig walks the KUBO_HARDENING.md validator table against
// the given Kubo config + mode. Returns the first violation as a
// wrapped ErrConfigViolation, naming the offending key. The validator
// stops at the first violation rather than collecting all of them so
// the operator's first restart sees the most upstream root cause.
//
// In ModePrivate, swarmKeyPath must point to a non-empty file
// containing the IPFS_SWARM_KEY format (per KUBO_HARDENING.md
// § "Private swarm key"). In ModePublicArchivalDHT, swarmKeyPath is
// ignored (pass "").
func ValidateConfig(cfg KuboConfig, mode Mode, swarmKeyPath string) error {
	// Rules that apply in BOTH modes.
	if err := requireLoopback("Addresses.API", cfg.Addresses.API); err != nil {
		return err
	}
	if err := requireLoopback("Addresses.Gateway", cfg.Addresses.Gateway); err != nil {
		return err
	}

	if mode == ModePublicArchivalDHT {
		// Public archival mode: loopback API/Gateway are all that's required.
		return nil
	}

	// Private mode rules.
	if cfg.Discovery.MDNS.Enabled {
		return wrapViolation("Discovery.MDNS.Enabled must be false in private mode")
	}
	if cfg.Provider.Strategy != "" {
		return wrapViolation("Provider.Strategy must be empty in private mode (got %q)", cfg.Provider.Strategy)
	}
	if cfg.Reprovider.Strategy != "" {
		return wrapViolation("Reprovider.Strategy must be empty in private mode (got %q)", cfg.Reprovider.Strategy)
	}
	switch cfg.Routing.Type {
	case "none", "dhtclient":
		// ok
	default:
		return wrapViolation("Routing.Type must be 'none' or 'dhtclient' in private mode (got %q)", cfg.Routing.Type)
	}
	if !cfg.Swarm.DisableNatPortMap {
		return wrapViolation("Swarm.DisableNatPortMap must be true in private mode")
	}
	for _, addr := range cfg.Bootstrap {
		if err := requirePrivateBootstrap(addr); err != nil {
			return err
		}
	}

	// Swarm key must exist and be non-empty.
	if swarmKeyPath == "" {
		return fmt.Errorf("validate: swarm key path is empty: %w", ErrSwarmKeyMissing)
	}
	info, err := os.Stat(swarmKeyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("validate: %s: %w", swarmKeyPath, ErrSwarmKeyMissing)
		}
		return fmt.Errorf("validate: stat %s: %w", swarmKeyPath, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("validate: %s is empty: %w", swarmKeyPath, ErrSwarmKeyMissing)
	}

	return nil
}

func requireLoopback(field, ma string) error {
	// Multiaddr strings start with /ip4/<ip>/tcp/... or /ip6/<ip>/tcp/...
	parts := strings.Split(ma, "/")
	if len(parts) < 3 {
		return wrapViolation("%s is not a valid multiaddr: %q", field, ma)
	}
	switch parts[1] {
	case "ip4":
		if parts[2] != "127.0.0.1" {
			return wrapViolation("%s must bind to 127.0.0.1 (got %q)", field, ma)
		}
	case "ip6":
		if parts[2] != "::1" {
			return wrapViolation("%s must bind to ::1 (got %q)", field, ma)
		}
	default:
		return wrapViolation("%s must be /ip4/ or /ip6/ (got %q)", field, ma)
	}
	return nil
}

// requirePrivateBootstrap accepts loopback, RFC 1918, or Nova-overlay
// addresses. (Nebula overlay support is recognised as RFC 1918 by
// default; operators who use non-RFC-1918 overlay subnets are out of
// scope for the table-driven test but the rule still applies — they
// pass an additional allow-list via a future config option, not in M2.)
func requirePrivateBootstrap(ma string) error {
	parts := strings.Split(ma, "/")
	if len(parts) < 3 {
		return wrapViolation("Bootstrap entry not a valid multiaddr: %q", ma)
	}
	switch parts[1] {
	case "ip4":
		ip := net.ParseIP(parts[2])
		if ip == nil {
			return wrapViolation("Bootstrap entry has bad IPv4 %q", parts[2])
		}
		if isLoopback4(ip) || isRFC1918(ip) {
			return nil
		}
		return wrapViolation("Bootstrap entry %q is not loopback/RFC1918 (private mode requires private addresses)", ma)
	case "ip6":
		ip := net.ParseIP(parts[2])
		if ip == nil {
			return wrapViolation("Bootstrap entry has bad IPv6 %q", parts[2])
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return nil
		}
		return wrapViolation("Bootstrap entry %q is not loopback IPv6", ma)
	case "dnsaddr", "dns4", "dns6", "dns":
		// DNS-bootstrap addresses cannot be resolved at config-validate
		// time without leaking the federation to the resolver. We refuse
		// them in private mode; operators who genuinely need DNS-based
		// private bootstraps run an internal resolver and use the ip4/ip6
		// form against its result.
		return wrapViolation("Bootstrap entry uses DNS resolution (%q); private mode requires literal IPs", ma)
	default:
		return wrapViolation("Bootstrap entry has unknown protocol %q", ma)
	}
}

func isLoopback4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return v4[0] == 127
}

func isRFC1918(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	default:
		return false
	}
}

func wrapViolation(format string, args ...any) error {
	return errors.Join(ErrConfigViolation, fmt.Errorf(format, args...))
}

// WriteFileForTest is a small re-export of os.WriteFile so the
// validate_test.go file can sit in the _test package. (Tests live in
// internal/ipfs_test to enforce the public surface; we expose a helper
// to keep the test code free of build-tag tricks.)
func WriteFileForTest(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
