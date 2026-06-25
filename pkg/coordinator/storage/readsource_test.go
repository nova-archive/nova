package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/ipfs"
)

// These are white-box unit tests (package storage) so they can exercise the
// donor-fetch seam without Postgres or Kubo. The DB reads are abstracted behind
// donorQuerier and the network behind donorFetcher; the backend is a tiny
// content-addressed echo store that reproduces AddDeterministic's canonical
// CIDv1(raw, sha2-256) so verify-before-decrypt is real.

// mkRawCID returns the canonical CIDv1(raw, sha2-256) string for data, matching
// what echoBackend.AddDeterministic computes.
func mkRawCID(t *testing.T, data []byte) string {
	t.Helper()
	mh, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return gocid.NewCidV1(gocid.Raw, mh).String()
}

// echoBackend is a minimal in-memory ipfs.Backend. AddDeterministic computes the
// real canonical CID for the bytes (so a CID mismatch on tampered bytes is a
// genuine verification failure, not a stub), pins them, and Has/Get reflect the
// pinned set. Only the methods OpenBytes/selectAndFetch touch are functional.
type echoBackend struct {
	mu     sync.Mutex
	store  map[string][]byte // cid -> bytes
	addErr error
}

func newEchoBackend() *echoBackend { return &echoBackend{store: map[string][]byte{}} }

func (b *echoBackend) put(cidStr string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store[cidStr] = data
}

func (b *echoBackend) AddDeterministic(_ context.Context, env []byte) (ipfs.AddResult, error) {
	if b.addErr != nil {
		return ipfs.AddResult{}, b.addErr
	}
	mh, err := multihash.Sum(env, multihash.SHA2_256, -1)
	if err != nil {
		return ipfs.AddResult{}, err
	}
	c := gocid.NewCidV1(gocid.Raw, mh)
	b.mu.Lock()
	b.store[c.String()] = append([]byte(nil), env...)
	b.mu.Unlock()
	return ipfs.AddResult{CID: c, MerkleRoot: c, EnvelopeSize: int64(len(env)), Codec: "raw"}, nil
}

func (b *echoBackend) Get(_ context.Context, c gocid.Cid) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.store[c.String()]
	if !ok {
		return nil, errors.New("echo: not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *echoBackend) Has(_ context.Context, c gocid.Cid) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.store[c.String()]
	return ok, nil
}

// has is a string-keyed convenience for cache tests asserting (un)pin state.
func (b *echoBackend) has(cidStr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.store[cidStr]
	return ok
}

func (b *echoBackend) Pin(context.Context, gocid.Cid) error { return nil }

// Unpin removes the pin (and the bytes) from the in-memory store so Has/has
// reflect the unpin — the transient-mode and eviction tests observe this.
func (b *echoBackend) Unpin(_ context.Context, c gocid.Cid) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.store, c.String())
	return nil
}
func (b *echoBackend) BlockstoreHas(context.Context, gocid.Cid) (bool, error) { return false, nil }
func (b *echoBackend) BlockGet(context.Context, gocid.Cid) ([]byte, error) {
	return nil, errors.New("unused")
}
func (b *echoBackend) Close(context.Context) error  { return nil }
func (b *echoBackend) Health(context.Context) error { return nil }

// fakeFetcher records its calls and returns canned bytes keyed by addr.
//
// gate, when non-nil, lets a test hold every Fetch call inside the fetcher until
// the test signals (by closing the channel). It is the synchronization hook the
// single-flight test uses to guarantee N callers are genuinely concurrent before
// any one of them completes — without it, callers might serialize and the
// collapse would be untestable.
type fakeFetcher struct {
	mu      sync.Mutex
	byAddr  map[string][]byte // addr -> bytes returned
	err     error             // returned for any addr not in byAddr
	calls   []fetchCall
	gate    chan struct{} // if non-nil, Fetch blocks until this is closed
	entered chan struct{} // if non-nil, Fetch sends once on entry (before gating)
}

type fetchCall struct {
	addr, cid, grant string
}

func (f *fakeFetcher) Fetch(_ context.Context, addr, cid, grant string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fetchCall{addr: addr, cid: cid, grant: grant})
	data, ok := f.byAddr[addr]
	entered, gate := f.entered, f.gate
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if gate != nil {
		<-gate
	}
	if !ok {
		if f.err != nil {
			return nil, f.err
		}
		return nil, errors.New("fetch: no canned response for " + addr)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// callsForAddr counts recorded Fetch calls targeting a specific donor addr.
func (f *fakeFetcher) callsForAddr(addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.addr == addr {
			n++
		}
	}
	return n
}

// fakeQuerier implements donorQuerier without a pool.
type fakeQuerier struct {
	envSize  int64
	envErr   error
	holders  []gen.ListSourceableHoldersRow
	holdErr  error
	admitted []gen.AdmitToCacheParams
	touched  []string
}

func (q *fakeQuerier) GetEnvelopeSize(context.Context, string) (int64, error) {
	return q.envSize, q.envErr
}

func (q *fakeQuerier) ListSourceableHolders(_ context.Context, _ gen.ListSourceableHoldersParams) ([]gen.ListSourceableHoldersRow, error) {
	return q.holders, q.holdErr
}

func (q *fakeQuerier) AdmitToCache(_ context.Context, arg gen.AdmitToCacheParams) error {
	q.admitted = append(q.admitted, arg)
	return nil
}

func (q *fakeQuerier) TouchLastAccessed(_ context.Context, arg gen.TouchLastAccessedParams) error {
	q.touched = append(q.touched, arg.Cid)
	return nil
}

func holderRow(addr string, rep float32) gen.ListSourceableHoldersRow {
	return gen.ListSourceableHoldersRow{
		NodeID:           pgtype.UUID{Bytes: uuid.New(), Valid: true},
		AssignmentID:     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Generation:       1,
		SourceNebulaAddr: pgtype.Text{String: addr, Valid: true},
		ReputationScore:  rep,
	}
}

// newTestSigner builds a real Ed25519 signer (no DB) for grant minting.
func newTestSigner(t *testing.T) *tokens.Signer {
	t.Helper()
	seed := bytes.Repeat([]byte{0x42}, 32)
	s, err := tokens.NewSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// plaintextView builds an unencrypted BlobView so OpenBytes streams directly
// from the backend after ensureLocal — keeping the test free of the keystore.
func plaintextView(cidStr string, size int64) *BlobView {
	return &BlobView{CID: cidStr, MIME: "application/octet-stream", PlaintextSize: size, Visibility: VisibilityPublic, Encrypted: false}
}

func TestOpenBytesLocalHitNoFetch(t *testing.T) {
	ctx := context.Background()
	data := []byte("already-local plaintext bytes")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend()
	be.put(cidStr, data)

	fetch := &fakeFetcher{byAddr: map[string][]byte{}}
	q := &fakeQuerier{}
	svc := &Service{backend: be}
	svc.setDonorReadSourceForTest(fetch, newTestSigner(t), q, time.Minute, 86400)

	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatalf("served %q, want %q", got, data)
	}
	if fetch.callCount() != 0 {
		t.Fatalf("local hit must not fetch from a donor; got %d fetches", fetch.callCount())
	}
}

func TestOpenBytesDonorMissFetchesVerifiesServes(t *testing.T) {
	ctx := context.Background()
	data := []byte("ciphertext envelope served by the donor")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend() // empty: local MISS

	fetch := &fakeFetcher{byAddr: map[string][]byte{"donor-a:4242": data}}
	q := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-a:4242", 0.9)}}
	svc := &Service{backend: be}
	svc.setDonorReadSourceForTest(fetch, newTestSigner(t), q, time.Minute, 86400)

	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if err != nil {
		t.Fatalf("OpenBytes: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatalf("served %q, want %q", got, data)
	}
	if fetch.callCount() != 1 {
		t.Fatalf("expected exactly one donor fetch, got %d", fetch.callCount())
	}
	if len(q.admitted) != 1 || q.admitted[0].Cid != cidStr {
		t.Fatalf("expected AdmitToCache for %s, got %+v", cidStr, q.admitted)
	}
	if q.admitted[0].LocalBytes != int64(len(data)) {
		t.Fatalf("admitted local_bytes=%d, want %d", q.admitted[0].LocalBytes, len(data))
	}
}

func TestDonorByteTamperRejectedAdvancesToNextHolder(t *testing.T) {
	ctx := context.Background()
	good := []byte("the authentic envelope bytes for this cid")
	cidStr := mkRawCID(t, good)
	tampered := []byte("MALICIOUS substitute bytes with a different hash")

	be := newEchoBackend() // local MISS

	// Holder A (higher reputation) returns tampered bytes; holder B returns good.
	fetch := &fakeFetcher{byAddr: map[string][]byte{
		"donor-bad:4242":  tampered,
		"donor-good:4242": good,
	}}
	q := &fakeQuerier{
		envSize: int64(len(good)),
		holders: []gen.ListSourceableHoldersRow{
			holderRow("donor-bad:4242", 0.99),
			holderRow("donor-good:4242", 0.50),
		},
	}
	svc := &Service{backend: be}
	svc.setDonorReadSourceForTest(fetch, newTestSigner(t), q, time.Minute, 86400)

	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(good))))
	if err != nil {
		t.Fatalf("OpenBytes should recover by advancing to the next holder: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, good) {
		t.Fatalf("served %q, want the authentic %q (tampered bytes must never be served)", got, good)
	}
	if fetch.callCount() != 2 {
		t.Fatalf("expected 2 fetches (bad then good), got %d", fetch.callCount())
	}
	// The tampered CID must not have been admitted; only the verified one.
	if len(q.admitted) != 1 || q.admitted[0].Cid != cidStr {
		t.Fatalf("only the verified blob may be admitted, got %+v", q.admitted)
	}
}

func TestOpenBytesNoSourceableHolders(t *testing.T) {
	ctx := context.Background()
	data := []byte("nobody holds this")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend() // local MISS
	fetch := &fakeFetcher{byAddr: map[string][]byte{}}
	q := &fakeQuerier{envSize: int64(len(data)), holders: nil} // no holders
	svc := &Service{backend: be}
	svc.setDonorReadSourceForTest(fetch, newTestSigner(t), q, time.Minute, 86400)

	_, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if !errors.Is(err, ErrNoSourceableHolder) {
		t.Fatalf("err = %v, want ErrNoSourceableHolder", err)
	}
	if fetch.callCount() != 0 {
		t.Fatalf("no holders means no fetch, got %d", fetch.callCount())
	}
}

func TestOpenBytesDonorFetchNotConfiguredMissIsNotFound(t *testing.T) {
	ctx := context.Background()
	data := []byte("local miss, donor-fetch disabled")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend() // local MISS, no donor-fetch configured
	svc := &Service{backend: be}

	_, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("err = %v, want ErrBlobNotFound (today's behavior)", err)
	}
}

// testReadSourceCfg returns a permissive containment config for white-box tests:
// generous timeout/bulkhead so the mechanism under test is the only constraint.
func testReadSourceCfg() ReadSourceConfig {
	return ReadSourceConfig{
		TTL:              time.Minute,
		StaleSecs:        86400,
		Timeout:          5 * time.Second,
		Bulkhead:         16,
		PerDonorLimit:    4,
		BreakerThreshold: 5,
		BreakerCooldown:  30 * time.Second,
		MaxFallbacks:     3,
	}
}

// TestSingleFlightCollapsesMisses launches N concurrent OpenBytes calls for one
// CID against a single holder. The donor fetch is gated so all N callers are
// in-flight at the miss before any completes; single-flight must collapse them
// into exactly one Fetch.
func TestSingleFlightCollapsesMisses(t *testing.T) {
	ctx := context.Background()
	data := []byte("one cid, many concurrent readers, one fetch")
	cidStr := mkRawCID(t, data)

	be := newEchoBackend() // local MISS

	const n = 8
	gate := make(chan struct{})
	entered := make(chan struct{}, n)
	fetch := &fakeFetcher{
		byAddr:  map[string][]byte{"donor-a:4242": data},
		gate:    gate,
		entered: entered,
	}
	q := &fakeQuerier{envSize: int64(len(data)), holders: []gen.ListSourceableHoldersRow{holderRow("donor-a:4242", 0.9)}}
	svc := &Service{backend: be}
	svc.setDonorReadSourceCfgForTest(fetch, newTestSigner(t), q, testReadSourceCfg())

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
			errs[i] = err
			if err == nil {
				_, _ = io.ReadAll(rc)
				rc.Close()
			}
		}(i)
	}

	// Wait until at least one caller has entered Fetch, then give the others a
	// brief moment to pile up on the same key, then release the gate.
	<-entered
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: OpenBytes: %v", i, err)
		}
	}
	if fetch.callCount() != 1 {
		t.Fatalf("single-flight must collapse %d concurrent misses into 1 fetch, got %d", n, fetch.callCount())
	}
}

// TestCircuitBreakerSkipsHolder drives a holder to fail K times so its breaker
// opens, then verifies a subsequent attempt SKIPS it (no further Fetch call for
// that addr) and falls through to a healthy holder.
func TestCircuitBreakerSkipsHolder(t *testing.T) {
	ctx := context.Background()
	data := []byte("breaker test envelope bytes for this content id")
	cidStr := mkRawCID(t, data)

	const badAddr = "donor-bad:4242"
	const goodAddr = "donor-good:4242"

	// badAddr is never in byAddr (so Fetch errors); goodAddr serves good bytes.
	fetch := &fakeFetcher{byAddr: map[string][]byte{goodAddr: data}, err: errors.New("connection refused")}
	cfg := testReadSourceCfg()
	cfg.BreakerThreshold = 3
	cfg.MaxFallbacks = 10 // don't let fallback bound interfere with the breaker

	svc := &Service{backend: newEchoBackend()}
	svc.setDonorReadSourceCfgForTest(fetch, newTestSigner(t), q0(int64(len(data)), badAddr), cfg)

	// Drive the bad holder to K failures. Each call has only the bad holder, so
	// it returns ErrNoSourceableHolder and increments the breaker once.
	for i := 0; i < cfg.BreakerThreshold; i++ {
		_, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
		if !errors.Is(err, ErrNoSourceableHolder) {
			t.Fatalf("attempt %d: err = %v, want ErrNoSourceableHolder", i, err)
		}
	}
	failsBeforeOpen := fetch.callsForAddr(badAddr)
	if failsBeforeOpen != cfg.BreakerThreshold {
		t.Fatalf("expected %d fetches to the bad holder before the breaker opens, got %d", cfg.BreakerThreshold, failsBeforeOpen)
	}

	// Now offer BOTH holders (bad first). The bad holder's breaker is open, so it
	// must be SKIPPED (no new Fetch), and the good holder serves the bytes.
	svc.donor.q = q2(int64(len(data)), badAddr, goodAddr)
	rc, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if err != nil {
		t.Fatalf("OpenBytes should recover via the healthy holder: %v", err)
	}
	_, _ = io.ReadAll(rc)
	rc.Close()

	if got := fetch.callsForAddr(badAddr); got != failsBeforeOpen {
		t.Fatalf("open breaker must skip the bad holder (no new fetch): had %d, now %d", failsBeforeOpen, got)
	}
	if got := fetch.callsForAddr(goodAddr); got != 1 {
		t.Fatalf("healthy holder should be fetched exactly once, got %d", got)
	}
}

// TestFallbackBounded offers more failing holders than max_fallbacks and asserts
// only max_fallbacks fetch ATTEMPTS are made (the request gives up rather than
// walking an unbounded candidate list).
func TestFallbackBounded(t *testing.T) {
	ctx := context.Background()
	data := []byte("nobody serves this; fallback must be bounded")
	cidStr := mkRawCID(t, data)

	addrs := []string{"d1:1", "d2:1", "d3:1", "d4:1", "d5:1", "d6:1"}
	rows := make([]gen.ListSourceableHoldersRow, 0, len(addrs))
	for i, a := range addrs {
		rows = append(rows, holderRow(a, float32(1.0)-float32(i)*0.1))
	}

	// All holders fail (none in byAddr; err set so Fetch returns an error).
	fetch := &fakeFetcher{byAddr: map[string][]byte{}, err: errors.New("unreachable")}
	q := &fakeQuerier{envSize: int64(len(data)), holders: rows}
	cfg := testReadSourceCfg()
	cfg.MaxFallbacks = 3

	svc := &Service{backend: newEchoBackend()}
	svc.setDonorReadSourceCfgForTest(fetch, newTestSigner(t), q, cfg)

	_, err := svc.OpenBytes(ctx, plaintextView(cidStr, int64(len(data))))
	if !errors.Is(err, ErrNoSourceableHolder) {
		t.Fatalf("err = %v, want ErrNoSourceableHolder", err)
	}
	if fetch.callCount() != cfg.MaxFallbacks {
		t.Fatalf("fallback must be bounded at %d attempts, got %d", cfg.MaxFallbacks, fetch.callCount())
	}
}

// q0 builds a fakeQuerier advertising exactly one holder at addr.
func q0(envSize int64, addr string) *fakeQuerier {
	return &fakeQuerier{envSize: envSize, holders: []gen.ListSourceableHoldersRow{holderRow(addr, 0.9)}}
}

// q2 builds a fakeQuerier advertising two holders (first then second).
func q2(envSize int64, a, b string) *fakeQuerier {
	return &fakeQuerier{envSize: envSize, holders: []gen.ListSourceableHoldersRow{holderRow(a, 0.99), holderRow(b, 0.5)}}
}
