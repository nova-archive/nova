package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"

	"github.com/nova-archive/nova/internal/federation/wire"
)

// P2-M7 disk-full drill (D-M7-4): a full disk during the Kubo import is a
// CLEAN, classified out_of_space refusal on the wire — never a generic
// kubo_error, and never a partial local-state write (progress is persisted
// only after Verify succeeds; a Verify failure leaves nothing behind).

type enospcFetcher struct{ body []byte }

func (f enospcFetcher) Fetch(context.Context, wire.ChangeSource, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

type enospcPinner struct{}

func (enospcPinner) AddDeterministic(context.Context, []byte) (string, error) {
	// Wrap the raw errno the way a real filesystem error surfaces (fs layer
	// wraps syscall.ENOSPC; errors.Is unwraps it).
	return "", fmt.Errorf("kubo import: write blocks: %w", syscall.ENOSPC)
}

func TestDrillOutOfSpaceIsCleanRefusal(t *testing.T) {
	env := []byte("envelope-bytes-that-do-not-fit")
	err := Verify(context.Background(), enospcFetcher{body: env}, enospcPinner{},
		wire.ChangeSource{}, "bafyENOSPC", int64(len(env)))
	if err == nil {
		t.Fatal("Verify must fail when the import hits ENOSPC")
	}
	var fe *FailErr
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want *FailErr", err)
	}
	if fe.Reason != wire.FailReasonOutOfSpace {
		t.Fatalf("reason = %q, want %q (out_of_space is the defined wire refusal)", fe.Reason, wire.FailReasonOutOfSpace)
	}
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("the classified error must still unwrap to ENOSPC for local diagnostics")
	}
}

// TestDrillNonSpaceKuboErrorStaysKuboError guards the classification boundary:
// only ENOSPC maps to out_of_space; other import failures remain kubo_error.
func TestDrillNonSpaceKuboErrorStaysKuboError(t *testing.T) {
	fail := func(context.Context, []byte) (string, error) { return "", errors.New("daemon unreachable") }
	err := Verify(context.Background(), enospcFetcher{body: []byte("x")}, pinnerFunc(fail),
		wire.ChangeSource{}, "bafyKUBOERR", 1)
	var fe *FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonKuboError {
		t.Fatalf("err = %v, want kubo_error classification", err)
	}
}

type pinnerFunc func(context.Context, []byte) (string, error)

func (f pinnerFunc) AddDeterministic(ctx context.Context, env []byte) (string, error) {
	return f(ctx, env)
}
