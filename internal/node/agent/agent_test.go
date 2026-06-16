package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/state"
	"github.com/stretchr/testify/require"
)

func TestAgentRunStopsOnContextCancel(t *testing.T) {
	a := agent.New(&nodeconfig.Config{}, state.NewMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("agent.Run did not return after cancel")
	}
}
