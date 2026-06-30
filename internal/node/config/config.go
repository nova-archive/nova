// Package config defines and validates the donor (nova-node) configuration. It
// is DONOR-LOCAL and deliberately separate from internal/config (the operator
// config home) so cmd/node never imports operator code. All secret material is
// referenced by *_path fields: node.yaml carries filesystem paths, never inline
// secret bytes. Validation is intentionally SHALLOW — it checks references, not
// cert chains — so a build-boundary milestone does not become cert provisioning.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultHealthListenAddr binds the M1 health endpoint to loopback (there is no
// Nebula interface yet; the federation listener binds the overlay from M2).
const DefaultHealthListenAddr = "127.0.0.1:9100"

// FailureDomain holds operator-DECLARED anti-affinity hints. They are
// informational at the donor and become authoritative only when
// operator-verified at the coordinator (D8).
type FailureDomain struct {
	Provider string `yaml:"provider"`
	ASN      string `yaml:"asn"`
	Region   string `yaml:"region"`
}

// Config is the donor node.yaml schema.
type Config struct {
	CoordinatorURL             string        `yaml:"coordinator_url"`
	FederationCAPath           string        `yaml:"federation_ca_path"`
	FederationCertPath         string        `yaml:"federation_cert_path"`
	FederationKeyPath          string        `yaml:"federation_key_path"`
	NebulaCertPath             string        `yaml:"nebula_cert_path"`
	NebulaKeyPath              string        `yaml:"nebula_key_path"`
	SwarmKeyPath               string        `yaml:"swarm_key_path"`
	StorageDir                 string        `yaml:"storage_dir"`
	BandwidthBudgetBytesPerDay int64         `yaml:"bandwidth_budget_bytes_per_day"`
	FailureDomain              FailureDomain `yaml:"failure_domain"`
	HealthListenAddr           string        `yaml:"health_listen_addr"`
	StorageMaxBytes            int64         `yaml:"storage_max_bytes"`           // 0 ⇒ unlimited (M4 enforces out_of_space)
	KuboAPIAddr                string        `yaml:"kubo_api_addr"`               // loopback Kubo sidecar HTTP API
	SourceNebulaAddr           string        `yaml:"source_nebula_addr"`          // M4.1: address coordinator uses to reach this donor's read-source server; empty = not a read source
	SourceReadListenAddr       string        `yaml:"source_read_listen_addr"`     // M4.1: bind address for the read-source mTLS listener (DISTINCT from the advertised source_nebula_addr); required when source_nebula_addr is set
	EgressBudgetBytesPerDay    int64         `yaml:"egress_budget_bytes_per_day"` // M4.1: authoritative D11 read-source egress budget; defaults to bandwidth_budget_bytes_per_day when unset
	AuditBudgetFraction        float64       `yaml:"audit_budget_fraction"`       // M6: fraction of the D11 egress budget reserved for possession-audit responses; defaults to 0.01 when unset
}

// LoadFromFile reads, parses, defaults, and validates a node.yaml.
func LoadFromFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("node config: read %s: %w", path, err)
	}
	return LoadFromBytes(data)
}

// LoadFromBytes parses node.yaml, applies defaults, and validates.
func LoadFromBytes(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("node config: parse: %w", err)
	}
	if c.HealthListenAddr == "" {
		c.HealthListenAddr = DefaultHealthListenAddr
	}
	if c.KuboAPIAddr == "" {
		c.KuboAPIAddr = "http://127.0.0.1:5001"
	}
	// M4.1: a read-source donor with no explicit egress budget inherits the
	// general bandwidth budget. This is applied AFTER validate() rejects a
	// negative explicit value, so an unset (0) egress budget never reaches a
	// refuse-all bucket (NewDailyBucket(0) refuses all work).
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.SourceNebulaAddr != "" && c.EgressBudgetBytesPerDay == 0 {
		c.EgressBudgetBytesPerDay = c.BandwidthBudgetBytesPerDay
	}
	if c.AuditBudgetFraction == 0 {
		c.AuditBudgetFraction = 0.01
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.CoordinatorURL == "" {
		return fmt.Errorf("node config: coordinator_url is required")
	}
	if u, err := url.ParseRequestURI(c.CoordinatorURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("node config: coordinator_url %q is not a valid http(s) URL", c.CoordinatorURL)
	}
	files := map[string]string{
		"federation_ca_path":   c.FederationCAPath,
		"federation_cert_path": c.FederationCertPath,
		"federation_key_path":  c.FederationKeyPath,
		"nebula_cert_path":     c.NebulaCertPath,
		"nebula_key_path":      c.NebulaKeyPath,
		"swarm_key_path":       c.SwarmKeyPath,
	}
	for field, path := range files {
		if err := checkReadableFile(field, path); err != nil {
			return err
		}
	}
	if c.BandwidthBudgetBytesPerDay <= 0 {
		return fmt.Errorf("node config: bandwidth_budget_bytes_per_day must be positive")
	}
	if c.StorageMaxBytes < 0 {
		return fmt.Errorf("node config: storage_max_bytes must be >= 0")
	}
	if _, _, err := net.SplitHostPort(c.HealthListenAddr); err != nil {
		return fmt.Errorf("node config: health_listen_addr %q is not host:port: %w", c.HealthListenAddr, err)
	}
	if c.StorageDir == "" {
		return fmt.Errorf("node config: storage_dir is required")
	}
	if err := os.MkdirAll(c.StorageDir, 0o700); err != nil {
		return fmt.Errorf("node config: storage_dir %q not usable: %w", c.StorageDir, err)
	}
	if err := checkWritableDir(c.StorageDir); err != nil {
		return err
	}
	// M4.1 read-source: an advertised source address is meaningless without a
	// listener to bind, and a negative egress budget is invalid. The two are
	// DISTINCT (advertised overlay address vs local bind address) by design.
	if c.SourceNebulaAddr != "" {
		if c.SourceReadListenAddr == "" {
			return fmt.Errorf("node config: source_read_listen_addr is required when source_nebula_addr is set")
		}
		if _, _, err := net.SplitHostPort(c.SourceReadListenAddr); err != nil {
			return fmt.Errorf("node config: source_read_listen_addr %q is not host:port: %w", c.SourceReadListenAddr, err)
		}
	}
	if c.EgressBudgetBytesPerDay < 0 {
		return fmt.Errorf("node config: egress_budget_bytes_per_day must be >= 0")
	}
	if c.AuditBudgetFraction < 0 || c.AuditBudgetFraction > 1 {
		return fmt.Errorf("node config: audit_budget_fraction must be in [0, 1]")
	}
	return nil
}

// checkWritableDir confirms storage_dir is actually writable by the current
// uid — important because the donor container runs distroless-nonroot against a
// mounted volume. It writes and removes a probe file.
func checkWritableDir(dir string) error {
	probe := filepath.Join(dir, ".nova-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		return fmt.Errorf("node config: storage_dir %q not writable: %w", dir, err)
	}
	_ = os.Remove(probe)
	return nil
}

// HeartbeatIntervalSeconds is the donor's initial heartbeat cadence before the
// coordinator overrides it via config_updates. M2 default 300.
func (c *Config) HeartbeatIntervalSeconds() int { return 300 }

// PinsPollIntervalSeconds is the donor's initial pins-poll cadence before the
// coordinator overrides it via config_updates.
func (c *Config) PinsPollIntervalSeconds() int { return 600 }

// checkReadableFile verifies a *_path is set, exists, is a regular file (not a
// directory), and is readable. It does NOT parse the contents.
func checkReadableFile(field, path string) error {
	if path == "" {
		return fmt.Errorf("node config: %s is required", field)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("node config: %s %q: %w", field, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("node config: %s %q is a directory, want a file", field, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("node config: %s %q not readable: %w", field, path, err)
	}
	_ = f.Close()
	return nil
}
