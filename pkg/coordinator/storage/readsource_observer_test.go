package storage

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
)

// recReadObserver records the P2-M7 (D-M7-1) read-path observability calls.
type recReadObserver struct {
	fetches  []string // result:reason
	egress   []string
	selFails []string
}

func (r *recReadObserver) Fetch(result, reason string, _ float64) {
	r.fetches = append(r.fetches, result+":"+reason)
}
func (r *recReadObserver) EgressRefusal(reason string)    { r.egress = append(r.egress, reason) }
func (r *recReadObserver) SelectionFailure(reason string) { r.selFails = append(r.selFails, reason) }

// errFetcher always fails with the given error.
type errFetcher struct{ err error }

func (f errFetcher) Fetch(context.Context, string, string, string) (io.ReadCloser, error) {
	return nil, f.err
}

func TestReadObserverNoHoldersIsSelectionFailure(t *testing.T) {
	ctx := context.Background()
	data := []byte("bytes nobody holds")
	cidStr := mkRawCID(t, data)

	q := &fakeQuerier{envSize: int64(len(data))} // zero holders
	svc := &Service{backend: newEchoBackend()}
	svc.setDonorReadSourceForTest(&fakeFetcher{byAddr: map[string][]byte{}}, newTestSigner(t), q, time.Minute, 86400)
	obs := &recReadObserver{}
	svc.SetReadObserver(obs)

	if _, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data)))); err == nil {
		t.Fatal("expected miss")
	}
	if len(obs.selFails) != 1 || obs.selFails[0] != "no_sourceable_holder" {
		t.Fatalf("selection failures = %v, want [no_sourceable_holder]", obs.selFails)
	}
}

func TestReadObserverRecordsFetchOutcomes(t *testing.T) {
	ctx := context.Background()
	data := []byte("the envelope bytes for observer tests")
	cidStr := mkRawCID(t, data)

	// Success path.
	q := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-ok:4242", 0.9)}}
	svc := &Service{backend: newEchoBackend()}
	svc.setDonorReadSourceForTest(&fakeFetcher{byAddr: map[string][]byte{"donor-ok:4242": data}}, newTestSigner(t), q, time.Minute, 86400)
	obs := &recReadObserver{}
	svc.SetReadObserver(obs)
	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("served %q", got)
	}
	if len(obs.fetches) != 1 || obs.fetches[0] != "ok:none" {
		t.Fatalf("fetches = %v, want [ok:none]", obs.fetches)
	}

	// Failed fetch path (generic error): error recorded, then selection failure.
	q2 := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-dead:4242", 0.9)}}
	svc2 := &Service{backend: newEchoBackend()}
	svc2.setDonorReadSourceForTest(&fakeFetcher{byAddr: map[string][]byte{}}, newTestSigner(t), q2, time.Minute, 86400)
	obs2 := &recReadObserver{}
	svc2.SetReadObserver(obs2)
	if _, err := svc2.OpenBytes(ctx, plaintextView(cidStr, int64(len(data)))); err == nil {
		t.Fatal("expected miss")
	}
	if len(obs2.fetches) != 1 || obs2.fetches[0] != "error:fetch" {
		t.Fatalf("fetches = %v, want [error:fetch]", obs2.fetches)
	}
	if len(obs2.selFails) != 1 || obs2.selFails[0] != "all_holders_failed" {
		t.Fatalf("selection failures = %v, want [all_holders_failed]", obs2.selFails)
	}
}

func TestReadObserverRecordsEgressRefusal(t *testing.T) {
	ctx := context.Background()
	data := []byte("budget-limited donor bytes")
	cidStr := mkRawCID(t, data)

	q := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-429:4242", 0.9)}}
	svc := &Service{backend: newEchoBackend()}
	svc.setDonorReadSourceForTest(errFetcher{err: errDonorEgressRefused}, newTestSigner(t), q, time.Minute, 86400)
	obs := &recReadObserver{}
	svc.SetReadObserver(obs)

	if _, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data)))); err == nil {
		t.Fatal("expected miss")
	}
	if len(obs.egress) != 1 {
		t.Fatalf("egress refusals = %v, want exactly one", obs.egress)
	}
	if len(obs.fetches) != 1 || obs.fetches[0] != "error:egress_refused" {
		t.Fatalf("fetches = %v, want [error:egress_refused]", obs.fetches)
	}
}
