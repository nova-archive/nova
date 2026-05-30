package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// CommittedRef is the post-commit view passed to product OnCommitted hooks.
type CommittedRef struct {
	CID        string
	Product    string
	Visibility Visibility
}

// AnalyzeResult is what a WriteHook returns from Analyze.
type AnalyzeResult struct {
	Scan        ScanResult
	Transformed []byte                                                 // nil ⇒ store the original
	ResultMIME  string                                                 // set iff Transformed != nil
	Persist     func(ctx context.Context, tx pgx.Tx, cid string) error // side-table write in Put's tx; nil ⇒ none
}

// WriteHook is the product seam Service.Put calls. The coordinator adapts a
// product.Product to this interface (so storage never imports product).
type WriteHook interface {
	Analyze(ctx context.Context, pc PutContext, plaintext []byte) (AnalyzeResult, error)
	OnCommitted(ctx context.Context, ref CommittedRef)
}
