package kinds

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerivativePrewarmStubIsNoop(t *testing.T) {
	require.NoError(t, DerivativePrewarmStub(context.Background(), []byte(`{"cid":"bafy"}`)))
	require.Equal(t, "derivative_prewarm", KindDerivativePrewarm)
}
