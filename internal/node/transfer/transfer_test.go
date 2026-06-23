package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	gocid "github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/node/transfer"
)

func mkCID(t *testing.T, data []byte) string {
	t.Helper()
	mh, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return gocid.NewCidV1(gocid.Raw, mh).String()
}

type fakeFetcher struct {
	data   []byte
	status int
}

func (f fakeFetcher) Fetch(_ context.Context, _ wire.ChangeSource, _ string, _ int64) (io.ReadCloser, error) {
	switch f.status {
	case 404:
		return nil, transfer.ErrSourceMissing
	case 403:
		return nil, transfer.ErrSourceUnauthorized
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// fakePinner echoes a fixed root (or an error). It records the bytes it received
// so a test can assert AddDeterministic saw the full (untruncated) envelope.
type fakePinner struct {
	root string
	err  error
	got  []byte
}

func (p *fakePinner) AddDeterministic(_ context.Context, envelope []byte) (string, error) {
	p.got = append([]byte(nil), envelope...)
	return p.root, p.err
}

func TestVerifyMatch(t *testing.T) {
	p := &fakePinner{root: "bafyX"}
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("ciphertext")}, p,
		wire.ChangeSource{}, "bafyX", 1<<20)
	if err != nil {
		t.Fatalf("expected match, got %v", err)
	}
	if string(p.got) != "ciphertext" {
		t.Fatalf("pinner saw %q, want full envelope", p.got)
	}
}

func TestVerifyMismatchClassifiesCIDMismatch(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("x")}, &fakePinner{root: "bafyWRONG"},
		wire.ChangeSource{}, "bafyEXPECTED", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonCIDMismatch {
		t.Fatalf("want cid_mismatch, got %v", err)
	}
}

func TestVerifySource404ClassifiesBlobUnavailable(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{status: 404}, &fakePinner{},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonBlobUnavailable {
		t.Fatalf("want blob_unavailable, got %v", err)
	}
}

func TestVerifySource403ClassifiesSourceUnauthorized(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{status: 403}, &fakePinner{},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonSourceUnauthorized {
		t.Fatalf("want source_unauthorized, got %v", err)
	}
}

func TestVerifyKuboErrorClassified(t *testing.T) {
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("x")}, &fakePinner{err: errors.New("boom")},
		wire.ChangeSource{}, "bafyX", 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonKuboError {
		t.Fatalf("want kubo_error, got %v", err)
	}
}

func TestVerifyOversizeNotImported(t *testing.T) {
	p := &fakePinner{root: "bafyX"}
	err := transfer.Verify(context.Background(), fakeFetcher{data: bytes.Repeat([]byte("x"), 11)}, p,
		wire.ChangeSource{}, "bafyX", 10) // source served 11 bytes under a 10-byte grant
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonOther {
		t.Fatalf("want oversize FailErr(other), got %v", err)
	}
	if p.got != nil {
		t.Fatal("oversize source must NOT be imported (no truncated pin)")
	}
}

// TestVerifyMismatchRealCIDs exercises the got.Equals(want) == false branch with
// two valid, distinct CIDv1 strings. The placeholder-string test above covers the
// decode-failure path; this test covers the actual CID-comparison logic.
func TestVerifyMismatchRealCIDs(t *testing.T) {
	assigned := mkCID(t, []byte("expected"))
	computed := mkCID(t, []byte("actual"))
	// Sanity: they must differ as strings so the fast-path is skipped.
	if assigned == computed {
		t.Fatal("test setup: CIDs must be distinct")
	}
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("ciphertext")},
		&fakePinner{root: computed}, wire.ChangeSource{}, assigned, 1<<20)
	var fe *transfer.FailErr
	if !errors.As(err, &fe) || fe.Reason != wire.FailReasonCIDMismatch {
		t.Fatalf("want cid_mismatch, got %v", err)
	}
}

// TestVerifyMatchRealCIDs exercises the got.Equals(want) == true path (and the
// fast-path string-equality short-circuit) using a real CIDv1.
func TestVerifyMatchRealCIDs(t *testing.T) {
	cid := mkCID(t, []byte("same"))
	err := transfer.Verify(context.Background(), fakeFetcher{data: []byte("ciphertext")},
		&fakePinner{root: cid}, wire.ChangeSource{}, cid, 1<<20)
	if err != nil {
		t.Fatalf("expected match with real CIDs, got %v", err)
	}
}
