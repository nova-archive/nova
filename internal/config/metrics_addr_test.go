package config_test

import (
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/config"
)

// TestConfigMetricsListenAddr pins the D-M7-1 tri-state: absent key → default
// loopback enabled; explicit "" → disabled (a deliberate operator act); any
// other value → that address.
func TestConfigMetricsListenAddr(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		want    string
		enabled bool
	}{
		{"absent key defaults to loopback", minimalYAML, "127.0.0.1:2112", true},
		{"explicit empty disables",
			withCoordinatorKey(`metrics_listen_addr: ""`), "", false},
		{"explicit addr wins",
			withCoordinatorKey(`metrics_listen_addr: "127.0.0.1:9999"`), "127.0.0.1:9999", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := config.LoadFromBytes([]byte(c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			addr, enabled := cfg.EffectiveMetricsListenAddr()
			if addr != c.want || enabled != c.enabled {
				t.Fatalf("EffectiveMetricsListenAddr() = (%q, %v), want (%q, %v)",
					addr, enabled, c.want, c.enabled)
			}
		})
	}
}

func TestConfigMetricsListenAddrRejectsBadHostPort(t *testing.T) {
	_, err := config.LoadFromBytes([]byte(withCoordinatorKey(`metrics_listen_addr: "not-a-hostport"`)))
	if err == nil || !strings.Contains(err.Error(), "metrics_listen_addr") {
		t.Fatalf("err = %v, want metrics_listen_addr validation error", err)
	}
}

// withCoordinatorKey appends a key under minimalYAML's coordinator: block
// (yaml.v3 rejects duplicate top-level keys, so we extend the existing block).
func withCoordinatorKey(line string) string {
	return strings.Replace(minimalYAML,
		"coordinator:\n  public_ipfs_dht: false",
		"coordinator:\n  public_ipfs_dht: false\n  "+line, 1)
}
