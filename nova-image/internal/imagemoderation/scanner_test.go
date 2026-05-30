package imagemoderation

import (
	"context"
	"testing"

	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

func TestScanAllows(t *testing.T) {
	r := New().Scan(context.Background(), []byte("anything"))
	require.Equal(t, storage.ActionAllow, r.Action)
}
