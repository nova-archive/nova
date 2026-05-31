package kinds

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewDerivativePrewarmHandler(t *testing.T) {
	t.Run("decodes payload and calls prewarm fn with correct args", func(t *testing.T) {
		var gotParent string
		var gotPresets []string

		fn := func(_ context.Context, parent string, presets []string) error {
			gotParent = parent
			gotPresets = presets
			return nil
		}

		h := NewDerivativePrewarmHandler(fn)
		payload := []byte(`{"parent_cid":"bafy123","presets":["thumb","medium"]}`)
		err := h(context.Background(), payload)

		require.NoError(t, err)
		require.Equal(t, "bafy123", gotParent)
		require.Equal(t, []string{"thumb", "medium"}, gotPresets)
	})

	t.Run("propagates error from prewarm fn verbatim", func(t *testing.T) {
		sentinel := errors.New("prewarm transient failure")

		fn := func(_ context.Context, _ string, _ []string) error {
			return sentinel
		}

		h := NewDerivativePrewarmHandler(fn)
		payload := []byte(`{"parent_cid":"bafy456","presets":["thumb"]}`)
		err := h(context.Background(), payload)

		require.ErrorIs(t, err, sentinel)
	})

	t.Run("returns error and does not call fn on invalid JSON", func(t *testing.T) {
		called := false

		fn := func(_ context.Context, _ string, _ []string) error {
			called = true
			return nil
		}

		h := NewDerivativePrewarmHandler(fn)
		err := h(context.Background(), []byte("{not json"))

		require.Error(t, err)
		require.False(t, called)
	})
}

func TestKindDerivativePrewarmConstant(t *testing.T) {
	require.Equal(t, "derivative_prewarm", KindDerivativePrewarm)
}
