package reload_test

import (
	"sync"
	"testing"

	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
	"github.com/stretchr/testify/require"
)

func base() *config.Config { return &config.Config{} }

func TestSwapBumpsVersionAndStores(t *testing.T) {
	s := reload.New(base(), nil, nil)
	require.Equal(t, uint64(0), s.Version())
	next := base()
	next.Operator.Hostname = "h2"
	v := s.Swap(next)
	require.Equal(t, uint64(1), v)
	require.Equal(t, "h2", s.Load().Operator.Hostname)
}

func TestSwapAppliesEnvOverlay(t *testing.T) {
	overlay := func(c *config.Config) { c.Operator.Hostname = "env-wins" }
	s := reload.New(base(), overlay, map[string]struct{}{"operator.hostname": {}})
	next := base()
	next.Operator.Hostname = "yaml-value"
	s.Swap(next)
	require.Equal(t, "env-wins", s.Load().Operator.Hostname) // env overlay re-applied
	_, pinned := s.EnvPinned()["operator.hostname"]
	require.True(t, pinned)
}

func TestSubscribersFire(t *testing.T) {
	s := reload.New(base(), nil, nil)
	var got string
	s.Subscribe(func(_, n *config.Config) { got = n.Operator.Hostname })
	next := base()
	next.Operator.Hostname = "notified"
	s.Swap(next)
	require.Equal(t, "notified", got)
}

func TestConcurrentLoadDuringSwap(t *testing.T) {
	s := reload.New(base(), nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Load() }()
		go func() { defer wg.Done(); s.Swap(base()) }()
	}
	wg.Wait() // -race must stay clean
}
