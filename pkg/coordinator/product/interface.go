// Package product defines the content-type-specific product layer interface.
// It is part of pkg/coordinator's public surface but v0.x.y UNSTABLE until
// Phase 4 adapters are real consumers. storage MUST NOT import this package
// (the storage.WriteHook seam inverts the dependency).
package product

import (
	"context"
	"io"
	"io/fs"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/nova-archive/nova/pkg/coordinator/storage"
)

// Product is registered with the coordinator at boot; one singleton per process.
type Product interface {
	Name() string
	AcceptedMimeTypes() []string

	// AnalyzeUpload runs on plaintext before encryption. plaintext is valid for
	// the call only; the product MUST NOT retain it. A non-nil transformedPlaintext
	// is encrypted/stored instead of the original. scan.Action != ActionAllow
	// rejects the upload (no state committed).
	AnalyzeUpload(ctx context.Context, uc *UploadContext, plaintext io.Reader) (
		metadata Metadata, scan *storage.ScanResult, transformedPlaintext io.Reader, err error)

	OnCommitted(ctx context.Context, blob *storage.CommittedRef, metadata Metadata) error
	OnDelete(ctx context.Context, tx pgx.Tx, parentCID string, newState string) error
	RegisterRoutes(r chi.Router)
	Migrations() (fs.FS, string)
}

// UploadContext carries declared metadata from the upload request.
type UploadContext struct {
	DeclaredMimeType string
	Filename         string
	CollectionID     *string
	OwnerID          *string
	SourceIP         string
}

// Metadata is product-specific side-table metadata. Persist writes the row
// inside the storage core's write transaction (products own their side tables).
type Metadata interface {
	ProductName() string
	Persist(ctx context.Context, tx pgx.Tx, cid string) error
}
