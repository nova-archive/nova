// Package transfer is the donor's blob-fetch + verify seam: stream ciphertext
// from a source, re-import deterministically per IPFS_IMPORT_RULES, and compare
// the computed root CID to the assigned CID (D4). M1 ships only the interface;
// the streaming + re-import implementation lands in M4.
package transfer

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented marks the M1 stub.
var ErrNotImplemented = errors.New("transfer: not implemented until P2-M4")

// Verifier fetches and verifies a blob by deterministic re-import.
type Verifier interface {
	VerifyReimport(ctx context.Context, r io.Reader, expectCID string) error
}

type stub struct{}

// NewStub returns an M1 placeholder Verifier.
func NewStub() Verifier { return stub{} }

func (stub) VerifyReimport(context.Context, io.Reader, string) error { return ErrNotImplemented }
