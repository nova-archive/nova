package ipfs_test

import (
	"path/filepath"
	"testing"

	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/stretchr/testify/require"
)

// goodPrivate returns a KuboConfig that passes Private-mode validation.
// Each violation test starts from this and mutates one field.
func goodPrivate() ipfs.KuboConfig {
	return ipfs.KuboConfig{
		Bootstrap: []string{
			"/ip4/127.0.0.1/tcp/4001/p2p/12D3KooWExampleA",
			"/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWExampleB",
		},
		Routing: ipfs.RoutingConfig{Type: "none"},
		Discovery: ipfs.DiscoveryConfig{
			MDNS: ipfs.MDNSConfig{Enabled: false},
		},
		Provider:   ipfs.ProviderConfig{Strategy: ""},
		Reprovider: ipfs.ReproviderConfig{Strategy: ""},
		Addresses: ipfs.AddressesConfig{
			API:     "/ip4/127.0.0.1/tcp/5001",
			Gateway: "/ip4/127.0.0.1/tcp/8080",
		},
		Swarm: ipfs.SwarmConfig{DisableNatPortMap: true},
	}
}

func writeSwarmKey(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "swarm.key")
	const content = "/key/swarm/psk/1.0.0/\n/base16/\n" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	require.NoError(t, writeFile(t, path, content))
	return path
}

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return ipfs.WriteFileForTest(path, []byte(content))
}

func TestValidatePrivateAcceptsGoodConfig(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	require.NoError(t, ipfs.ValidateConfig(goodPrivate(), ipfs.ModePrivate, swarm))
}

func TestValidatePrivateRejectsMDNSEnabled(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Discovery.MDNS.Enabled = true
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Discovery.MDNS.Enabled")
}

func TestValidatePrivateRejectsProviderStrategy(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Provider.Strategy = "all"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Provider.Strategy")
}

func TestValidatePrivateRejectsReproviderStrategy(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	cfg := goodPrivate()
	cfg.Reprovider.Strategy = "all"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, swarm)
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Reprovider.Strategy")
}

func TestValidatePrivateRejectsRoutingType(t *testing.T) {
	t.Parallel()
	swarm := writeSwarmKey(t)
	for _, bad := range []string{"dht", "dhtserver", "auto", "autoclient"} {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Routing.Type = bad
			err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
			require.ErrorIs(t, err, ipfs.ErrConfigViolation)
			require.Contains(t, err.Error(), "Routing.Type")
		})
	}
	_ = swarm
}

func TestValidatePrivateAcceptsRoutingNoneOrDhtClient(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"none", "dhtclient"} {
		ok := ok
		t.Run(ok, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Routing.Type = ok
			require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t)))
		})
	}
}

func TestValidatePrivateRejectsAddressesAPINonLoopback(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.API = "/ip4/0.0.0.0/tcp/5001"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Addresses.API")
}

func TestValidatePrivateRejectsAddressesGatewayNonLoopback(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.Gateway = "/ip4/0.0.0.0/tcp/8080"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Addresses.Gateway")
}

func TestValidatePrivateRejectsNatPortMapEnabled(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Swarm.DisableNatPortMap = false
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
	require.Contains(t, err.Error(), "Swarm.DisableNatPortMap")
}

func TestValidatePrivateRejectsPublicBootstrap(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnoo",
		"/ip4/8.8.8.8/tcp/4001/p2p/12D3Koo",
		"/ip6/2001:db8::1/tcp/4001/p2p/12D3Koo",
		"/dns4/node.ipfs.io/tcp/4001/p2p/12D3Koo",
	} {
		addr := addr
		t.Run(addr, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Bootstrap = append(cfg.Bootstrap, addr)
			err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t))
			require.ErrorIs(t, err, ipfs.ErrConfigViolation)
			require.Contains(t, err.Error(), "Bootstrap")
		})
	}
}

func TestValidatePrivateAcceptsRFC1918Bootstrap(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{
		"/ip4/10.0.0.5/tcp/4001/p2p/12D3Koo",
		"/ip4/172.16.0.1/tcp/4001/p2p/12D3Koo",
		"/ip4/192.168.1.1/tcp/4001/p2p/12D3Koo",
		"/ip4/127.0.0.1/tcp/4001/p2p/12D3Koo",
		"/ip6/::1/tcp/4001/p2p/12D3Koo",
	} {
		addr := addr
		t.Run(addr, func(t *testing.T) {
			cfg := goodPrivate()
			cfg.Bootstrap = []string{addr}
			require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePrivate, writeSwarmKey(t)))
		})
	}
}

func TestValidatePrivateRefusesMissingSwarmKey(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, filepath.Join(t.TempDir(), "no-such-file"))
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}

func TestValidatePrivateRefusesEmptySwarmKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "swarm.key")
	require.NoError(t, ipfs.WriteFileForTest(empty, []byte{}))
	cfg := goodPrivate()
	err := ipfs.ValidateConfig(cfg, ipfs.ModePrivate, empty)
	require.ErrorIs(t, err, ipfs.ErrSwarmKeyMissing)
}

func TestValidatePublicArchivalDHTRelaxesMost(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Routing.Type = "dht"
	cfg.Provider.Strategy = "all"
	cfg.Reprovider.Strategy = "all"
	cfg.Bootstrap = append(cfg.Bootstrap, "/dnsaddr/bootstrap.libp2p.io/p2p/QmAny")
	cfg.Swarm.DisableNatPortMap = false
	// Loopback API and Gateway remain mandatory even in public mode.
	require.NoError(t, ipfs.ValidateConfig(cfg, ipfs.ModePublicArchivalDHT, ""))
}

func TestValidatePublicArchivalStillRequiresLoopbackAPI(t *testing.T) {
	t.Parallel()
	cfg := goodPrivate()
	cfg.Addresses.API = "/ip4/0.0.0.0/tcp/5001"
	err := ipfs.ValidateConfig(cfg, ipfs.ModePublicArchivalDHT, "")
	require.ErrorIs(t, err, ipfs.ErrConfigViolation)
}
