package coordinator

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	gocid "github.com/ipfs/go-cid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multiformats/go-multihash"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// fakeBackend implements the narrow blobSource interface. Get returns
// the canned bytes for its CID string, or an error for any other CID.
type fakeBackend struct {
	cid  string
	data []byte
}

func fakeBackendFor(cidStr string, data []byte) fakeBackend {
	return fakeBackend{cid: cidStr, data: data}
}

func (f fakeBackend) Get(_ context.Context, c gocid.Cid) (io.ReadCloser, error) {
	if c.String() != f.cid {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// mkCID returns a valid canonical CIDv1(raw, sha2-256) string for data.
func mkCID(t *testing.T, data []byte) string {
	t.Helper()
	mh, err := multihash.Sum(data, multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return gocid.NewCidV1(gocid.Raw, mh).String()
}

// insertBlobRow inserts a blob row and a matching blob_manifests row (with
// envelope_size = size) so GetEnvelopeSize and GetBlobSize both return a size.
// In production every committed blob has a manifest; tests should too.
func insertBlobRow(t *testing.T, pool *pgxpool.Pool, cidStr string, size int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO blobs (cid, mime_type, byte_size) VALUES ($1,'application/octet-stream',$2)`,
		cidStr, size)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(),
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1,'sha2-256','raw','size-262144',$2,$2,1)`,
		cidStr, size)
	if err != nil {
		t.Fatal(err)
	}
}

// blobSetup holds everything a blob sub-test needs: a server, the pool, the
// node id, and the leaf cert that was used to register (must be reused for
// subsequent requests so the fingerprint check passes).
type blobSetup struct {
	s    *Server
	pool *pgxpool.Pool
	id   uuid.UUID
	leaf *x509.Certificate // same cert used at registration time
}

// newBlobSetup creates a server with one registered donor.
func newBlobSetup(t *testing.T) blobSetup {
	t.Helper()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	return blobSetup{s: s, pool: pool, id: id, leaf: leaf}
}

// fire sends a GET /fed/v1/blob/{cidStr} using the registered leaf cert.
func (bs blobSetup) fire(t *testing.T, cidStr, tok string) *httptest.ResponseRecorder {
	t.Helper()
	r := reqWithCert(http.MethodGet, "/fed/v1/blob/"+cidStr, nil, bs.leaf)
	if tok != "" {
		r.Header.Set("X-Nova-Repair-Token", tok)
	}
	w := httptest.NewRecorder()
	bs.s.mux().ServeHTTP(w, r)
	return w
}

// mintTok mints a valid repair token.
func mintTok(t *testing.T, signer *tokens.Signer, jti, destID, cidStr string, notBefore, notAfter, maxBytes int64) string {
	t.Helper()
	tok, err := signer.Mint(wire.Claims{
		JTI: jti, AssignmentID: "a1", Generation: 1, CID: cidStr,
		SourceNodeID: tokens.ReservedCoordinatorSourceID, DestNodeID: destID,
		NotBefore: notBefore, NotAfter: notAfter,
		MaxBytes: maxBytes, ProtocolVersion: wire.ProtocolV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestBlobServeHappyPath(t *testing.T) {
	bs := newBlobSetup(t)
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("ciphertext-bytes")
	cidStr := mkCID(t, body)
	insertBlobRow(t, bs.pool, cidStr, int64(len(body)))
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	tok := mintTok(t, signer, "j-happy", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 1<<20)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusOK {
		t.Fatalf("happy path: status %d body %s", w.Code, w.Body)
	}
	got, _ := io.ReadAll(w.Body)
	if string(got) != string(body) {
		t.Fatalf("body %q != %q", got, body)
	}
}

func TestBlobRejectsWrongDest(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-wrong-dest")
	cidStr := mkCID(t, body)
	insertBlobRow(t, bs.pool, cidStr, int64(len(body)))
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	// Mint token for a DIFFERENT dest node, not the registered caller.
	wrongDest := uuid.New().String()
	tok := mintTok(t, signer, "j-wrong-dest", wrongDest, cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 1<<20)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong dest: status %d body %s", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.FailReasonSourceUnauthorized {
		t.Fatalf("wrong dest: code %q, want %q", er.Code, wire.FailReasonSourceUnauthorized)
	}
}

func TestBlobRejectsExpired(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-expired")
	cidStr := mkCID(t, body)
	insertBlobRow(t, bs.pool, cidStr, int64(len(body)))
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	// Token expired two minutes ago.
	past := time.Now().Add(-2 * time.Minute)
	tok := mintTok(t, signer, "j-expired", bs.id.String(), cidStr,
		past.Add(-10*time.Second).Unix(), past.Unix(), 1<<20)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expired: status %d body %s", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.FailReasonSourceUnauthorized {
		t.Fatalf("expired: code %q", er.Code)
	}
}

func TestBlobRejectsPreBootToken(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-preboot")
	cidStr := mkCID(t, body)
	insertBlobRow(t, bs.pool, cidStr, int64(len(body)))

	// Boot time is 10 minutes in the future relative to not_before → token predates boot.
	bootTime := time.Now().Add(10 * time.Minute)
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), bootTime)

	now := time.Now()
	tok := mintTok(t, signer, "j-preboot", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 1<<20)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("pre-boot: status %d body %s", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.FailReasonSourceUnauthorized {
		t.Fatalf("pre-boot: code %q", er.Code)
	}
}

func TestBlobRejectsReplay(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-replay")
	cidStr := mkCID(t, body)
	insertBlobRow(t, bs.pool, cidStr, int64(len(body)))
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	tok := mintTok(t, signer, "j-replay", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 1<<20)

	// First use: must succeed.
	w1 := bs.fire(t, cidStr, tok)
	if w1.Code != http.StatusOK {
		t.Fatalf("replay first use: status %d body %s", w1.Code, w1.Body)
	}

	// Second use of the same jti: must be rejected as replay.
	w2 := bs.fire(t, cidStr, tok)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("replay second use: status %d body %s", w2.Code, w2.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w2.Body.Bytes(), &er)
	if er.Code != wire.FailReasonSourceUnauthorized {
		t.Fatalf("replay second: code %q", er.Code)
	}
}

func TestBlobUnknownCID404(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-unknown")
	cidStr := mkCID(t, body)
	// Do NOT insert a blob row → GetBlobByteSize returns pgx.ErrNoRows → 404.
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	tok := mintTok(t, signer, "j-unknown", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 1<<20)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown CID: status %d body %s", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != wire.FailReasonBlobUnavailable {
		t.Fatalf("unknown CID: code %q", er.Code)
	}
}

func TestBlobOversizeRejected(t *testing.T) {
	bs := newBlobSetup(t)
	signer, _ := tokens.NewSignerFromSeed(make([]byte, 32))
	body := []byte("data-oversize")
	cidStr := mkCID(t, body)
	// Insert blob with a large byte_size (100 bytes) but token only allows 10.
	insertBlobRow(t, bs.pool, cidStr, 100)
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	tok := mintTok(t, signer, "j-oversize", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), 10)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: status %d body %s", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != "blob_too_large" {
		t.Fatalf("oversize: code %q, want %q", er.Code, "blob_too_large")
	}
}

// insertManifestRow seeds a blob_manifests row for tests that exercise the
// envelope-size path (GetEnvelopeSize / GetBlobSize / GetPinSnapshotPage).
func insertManifestRow(t *testing.T, pool *pgxpool.Pool, cidStr string, plaintextSize, envelopeSize int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO blob_manifests (cid, hash_alg, codec, chunker, plaintext_size, envelope_size, block_count)
		 VALUES ($1, 'sha2-256', 'raw', 'size-262144', $2, $3, 1)`,
		cidStr, plaintextSize, envelopeSize)
	if err != nil {
		t.Fatal(err)
	}
}

// TestSourcePreflightUsesEnvelopeSize verifies that the source preflight
// consults blob_manifests.envelope_size (not blobs.byte_size), so a grant
// whose max_bytes sits strictly between plaintext byte_size and envelope_size
// causes a 413 blob_too_large rejection before any body is written.
func TestSourcePreflightUsesEnvelopeSize(t *testing.T) {
	bs := newBlobSetup(t)
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("ciphertext-envelope-larger-than-plaintext")
	cidStr := mkCID(t, body)

	// plaintextSize < max_bytes < envelopeSize:
	// if the preflight uses byte_size (plaintext) it will ALLOW the transfer;
	// if it uses envelope_size it MUST REFUSE with 413.
	var (
		plaintextSize = int64(50)
		envelopeSize  = int64(200) // ciphertext overhead makes it larger
		maxBytes      = int64(100) // sits between plaintext and envelope
	)

	// Insert the blob row directly (without using insertBlobRow, which would
	// add a manifest with envelopeSize == plaintextSize, defeating the test).
	_, err = bs.pool.Exec(context.Background(),
		`INSERT INTO blobs (cid, mime_type, byte_size) VALUES ($1,'application/octet-stream',$2)`,
		cidStr, plaintextSize)
	if err != nil {
		t.Fatal(err)
	}
	insertManifestRow(t, bs.pool, cidStr, plaintextSize, envelopeSize)
	bs.s.SetSourceDeps(signer, fakeBackendFor(cidStr, body), time.Now().Add(-time.Minute))

	now := time.Now()
	tok := mintTok(t, signer, "j-envelope-size", bs.id.String(), cidStr,
		now.Add(-10*time.Second).Unix(), now.Add(5*time.Minute).Unix(), maxBytes)

	w := bs.fire(t, cidStr, tok)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("envelope preflight: status %d body %q (want 413 blob_too_large)", w.Code, w.Body)
	}
	var er wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &er)
	if er.Code != "blob_too_large" {
		t.Fatalf("envelope preflight: code %q, want \"blob_too_large\"", er.Code)
	}
	// The response body must have the error JSON only — no ciphertext bytes.
	if len(body) > 0 && bytes.Contains(w.Body.Bytes(), body) {
		t.Fatal("envelope preflight: ciphertext bytes written to response (must not write body on rejection)")
	}
}
