// Package transfer is the donor's fetch + verify path (D-M4-3): pull ciphertext
// from a source, re-import it deterministically via the Kubo sidecar, and compare
// the computed root CID to the assigned CID (D4). M4 is coordinator-as-source only.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nova-archive/nova/internal/federation/wire"
)

// ErrSourceMissing is returned by a SourceFetcher when the source has no such CID.
var ErrSourceMissing = errors.New("transfer: source missing cid")

// ErrSourceUnauthorized is returned when the source rejects the repair token.
var ErrSourceUnauthorized = errors.New("transfer: source unauthorized")

// FailErr carries a classified fail reason (wire.FailReason*) for /pins/{cid}/fail.
type FailErr struct {
	Reason string
	Err    error
}

func (e *FailErr) Error() string { return fmt.Sprintf("transfer failed (%s): %v", e.Reason, e.Err) }
func (e *FailErr) Unwrap() error { return e.Err }

// SourceFetcher fetches bytes for a CID from a designated source under a grant.
type SourceFetcher interface {
	Fetch(ctx context.Context, src wire.ChangeSource, cid string, maxBytes int64) (io.ReadCloser, error)
}

// Pinner deterministically re-imports + pins an envelope, returning the root CID
// ([]byte for parity with the embedded backend and the raw/dag-pb size branch).
type Pinner interface {
	AddDeterministic(ctx context.Context, envelope []byte) (string, error)
}

// Verify fetches, re-imports, and confirms the root CID equals cid. It reads at
// most maxBytes+1 and refuses an over-grant source WITHOUT importing it (so a
// misbehaving source can never get a truncated blob pinned + mislabeled
// cid_mismatch — important when this verifier is reused for M5 donor sources).
// On any failure it returns a *FailErr with the spec reason (D-M4-8).
func Verify(ctx context.Context, fetcher SourceFetcher, pinner Pinner, src wire.ChangeSource, cid string, maxBytes int64) error {
	rc, err := fetcher.Fetch(ctx, src, cid, maxBytes)
	if err != nil {
		switch {
		case errors.Is(err, ErrSourceMissing):
			return &FailErr{Reason: wire.FailReasonBlobUnavailable, Err: err}
		case errors.Is(err, ErrSourceUnauthorized):
			return &FailErr{Reason: wire.FailReasonSourceUnauthorized, Err: err}
		default:
			return &FailErr{Reason: wire.FailReasonNetworkError, Err: err}
		}
	}
	defer rc.Close()

	// Read up to maxBytes+1 so we can DETECT an over-grant response.
	envelope, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return &FailErr{Reason: wire.FailReasonNetworkError, Err: err}
	}
	if int64(len(envelope)) > maxBytes {
		return &FailErr{Reason: wire.FailReasonOther, Err: fmt.Errorf("source served > max_bytes (%d)", maxBytes)}
	}

	root, err := pinner.AddDeterministic(ctx, envelope)
	if err != nil {
		return &FailErr{Reason: wire.FailReasonKuboError, Err: err}
	}
	// Donor verification uses canonical CID-string equality, not general
	// (multibase-normalized) CID equality. Both sides use the shared importspec +
	// Kubo's canonical CIDv1 output, so a real match is byte-identical; normalization
	// is intentionally out of scope for the donor runtime (D-M4-6/D-M4-10).
	if root != cid {
		return &FailErr{Reason: wire.FailReasonCIDMismatch, Err: fmt.Errorf("root %s != assigned %s", root, cid)}
	}
	return nil
}
