package product_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/pkg/coordinator/product"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
	"github.com/stretchr/testify/require"
)

func TestScanResultAllowConst(t *testing.T) {
	require.Equal(t, "allow", string(storage.ActionAllow))
}

type imageMetaStub struct{}

func (imageMetaStub) ProductName() string                           { return "image" }
func (imageMetaStub) Persist(context.Context, pgx.Tx, string) error { return nil }

func TestMetadataImplements(t *testing.T) {
	var m product.Metadata = imageMetaStub{}
	require.Equal(t, "image", m.ProductName())
}
