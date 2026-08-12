package coordinator

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// The D-M7.3-7c state machine, one test per transition.

func heartbeatWith(t *testing.T, s *Server, leaf *x509.Certificate, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", body, leaf))
	return w
}

func contractBody(t *testing.T, c *wire.RuntimeContract) []byte {
	t.Helper()
	b, err := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1, StoredBytes: 2, RuntimeContract: c})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func nodeState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (observed bool, effective []string, version *string) {
	t.Helper()
	var ts *string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_contract_observed_at::text, effective_capabilities, reported_client_version
		FROM nodes WHERE id = $1::uuid`, id).Scan(&ts, &effective, &version); err != nil {
		t.Fatal(err)
	}
	return ts != nil, effective, version
}

func fullContract() *wire.RuntimeContract {
	return &wire.RuntimeContract{
		Version:          wire.RuntimeContractVersion,
		ClientVersion:    "v0.3.0",
		ImageDigest:      "sha256:" + strings.Repeat("a", 64),
		BundleLockDigest: "sha256:" + strings.Repeat("b", 64),
		Capabilities: []string{
			wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer, wire.CapReadSource,
		},
		Protocols: []string{wire.ProtocolV1},
	}
}

// TestRegistrationSnapshotStartsWithNoObservationEpoch.
func TestRegistrationSnapshotStartsWithNoObservationEpoch(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	registerOK(t, s, caPEM, caKeyPEM, id)

	observed, effective, version := nodeState(t, ctx, pool, id)
	if observed {
		t.Error("registration must leave the marker NULL; a stamped marker would make the " +
			"next contract-less heartbeat read as a rollback")
	}
	if !slices.Contains(effective, wire.CapBlobTransfer) {
		t.Errorf("effective_capabilities = %v, want the registration snapshot", effective)
	}
	if version != nil {
		t.Errorf("reported_client_version = %v before any contract", *version)
	}
}

// TestLegacyOmissionRetainsRegistrationSnapshot: marker NULL and no contract
// means a pre-M7.3 donor, and its snapshot stands.
func TestLegacyOmissionRetainsRegistrationSnapshot(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1})
	if w := heartbeatWith(t, s, leaf, body); w.Code != http.StatusOK {
		t.Fatalf("legacy heartbeat = %d (%s)", w.Code, w.Body)
	}

	observed, effective, _ := nodeState(t, ctx, pool, id)
	if observed {
		t.Error("a legacy heartbeat must not stamp the observation marker")
	}
	if !slices.Contains(effective, wire.CapBlobTransfer) {
		t.Errorf("effective_capabilities = %v; a legacy donor keeps its registration snapshot", effective)
	}
}

// TestSupportedContractAdoptsTheReportedSetAndStampsTheMarker.
func TestSupportedContractAdoptsTheReportedSetAndStampsTheMarker(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	if w := heartbeatWith(t, s, leaf, contractBody(t, fullContract())); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", w.Code, w.Body)
	}

	observed, effective, version := nodeState(t, ctx, pool, id)
	if !observed {
		t.Error("a supported contract must stamp the marker")
	}
	if !slices.Contains(effective, wire.CapReadSource) {
		t.Errorf("effective_capabilities = %v, want the contract's set", effective)
	}
	if version == nil || *version != "v0.3.0" {
		t.Errorf("reported_client_version = %v, want v0.3.0", version)
	}
}

// TestCapabilityAdditionTakesEffectWithoutReregistration. A donor with a durable
// registration never re-registers (agent.go), so before this the coordinator
// could not learn a new capability at all.
func TestCapabilityAdditionTakesEffectWithoutReregistration(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id) // registers without read-source

	if _, effective, _ := nodeState(t, ctx, pool, id); slices.Contains(effective, wire.CapReadSource) {
		t.Fatal("fixture registers without read-source; this test would prove nothing")
	}

	heartbeatWith(t, s, leaf, contractBody(t, fullContract()))

	_, effective, _ := nodeState(t, ctx, pool, id)
	if !slices.Contains(effective, wire.CapReadSource) {
		t.Error("an upgraded donor's new capability must take effect on the heartbeat; it has " +
			"no other way to tell the coordinator")
	}
}

// TestRollbackOmissionDropsOptionalRoleEligibility. Observed once, then absent:
// a downgrade. Optional-role capabilities go; replicas stay.
func TestRollbackOmissionDropsOptionalRoleEligibility(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	heartbeatWith(t, s, leaf, contractBody(t, fullContract()))
	if _, effective, _ := nodeState(t, ctx, pool, id); !slices.Contains(effective, wire.CapReadSource) {
		t.Fatal("setup: the contract should have granted read-source")
	}

	// The donor rolls back to a build that sends no contract.
	body, _ := json.Marshal(wire.HeartbeatRequest{FreeBytes: 1})
	if w := heartbeatWith(t, s, leaf, body); w.Code != http.StatusOK {
		t.Fatalf("rollback heartbeat = %d (%s)", w.Code, w.Body)
	}

	observed, effective, version := nodeState(t, ctx, pool, id)
	if !observed {
		t.Error("the marker must NOT be cleared; clearing it would make the next silence read " +
			"as legacy all over again")
	}
	if slices.Contains(effective, wire.CapReadSource) {
		t.Error("a rolled-back donor stays eligible as a read source for a role it may no " +
			"longer implement — this is the correctness fix, not a cosmetic one")
	}
	for _, core := range coreProfile() {
		if !slices.Contains(effective, core) {
			t.Errorf("core capability %s was dropped; only optional-role eligibility goes", core)
		}
	}
	if version != nil {
		t.Errorf("reported_client_version = %v, want cleared", *version)
	}
}

// TestReregistrationResetsObservationEpoch is the load-bearing transition:
// without it a legacy donor returning after eviction inherits a stamped marker
// and its next contract-less heartbeat is misread as a rollback.
func TestReregistrationResetsObservationEpoch(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	heartbeatWith(t, s, leaf, contractBody(t, fullContract()))
	if observed, _, _ := nodeState(t, ctx, pool, id); !observed {
		t.Fatal("setup: the marker should be stamped")
	}

	// Re-register with the SAME certificate — a new one would (correctly) be
	// refused as not the active cert, and this transition is about a donor
	// coming back, not a new enrollment.
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols: []string{wire.ProtocolV1},
		Capabilities:       []string{wire.CapPinChangeLog, wire.CapSnapshot, wire.CapBlobTransfer},
	})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("re-register = %d (%s)", w.Code, w.Body)
	}

	observed, effective, version := nodeState(t, ctx, pool, id)
	if observed {
		t.Error("re-registration must reset the observation epoch to NULL")
	}
	if version != nil {
		t.Errorf("reported_client_version = %v after re-registration", *version)
	}
	if !slices.Contains(effective, wire.CapBlobTransfer) {
		t.Errorf("effective_capabilities = %v, want the fresh registration snapshot", effective)
	}
}

// TestUnknownFutureContractVersionGrantsNoNewOptionalWork. Compatible heartbeat,
// no interpretation: conservative in the direction that loses work rather than
// the direction that loses data.
func TestUnknownFutureContractVersionGrantsNoNewOptionalWork(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	_, before, _ := nodeState(t, ctx, pool, id)

	future := fullContract()
	future.Version = wire.RuntimeContractVersion + 7
	future.Capabilities = append(future.Capabilities, "some-future-role/v9")
	w := heartbeatWith(t, s, leaf, contractBody(t, future))
	if w.Code != http.StatusOK {
		t.Fatalf("an unknown FUTURE version must keep the heartbeat compatible, got %d (%s)", w.Code, w.Body)
	}

	observed, after, version := nodeState(t, ctx, pool, id)
	if !observed {
		t.Error("a contract WAS observed, so the marker is stamped and a later silence is a downgrade")
	}
	if !slices.Equal(before, after) {
		t.Errorf("effective_capabilities changed from %v to %v; capabilities must not be "+
			"inferred from a schema this coordinator cannot parse", before, after)
	}
	if slices.Contains(after, "some-future-role/v9") {
		t.Error("a capability from an unparseable contract was granted")
	}
	if version != nil {
		t.Errorf("reported_client_version = %v; an unparseable contract's claims are not trusted", *version)
	}
}

// TestAllCapabilityPredicatesReadEffectiveCapabilities. A second source is how a
// role predicate silently keeps consulting a stale set.
func TestAllCapabilityPredicatesReadEffectiveCapabilities(t *testing.T) {
	root := filepath.Join("..", "..", "db", "queries")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if strings.Contains(line, "advertised_capabilities @>") {
				t.Errorf("%s:%d gates on advertised_capabilities; effective_capabilities is the "+
					"single operational source (D-M7.3-6a):\n  %s", e.Name(), i+1, trimmed)
			}
		}
	}
}

// TestDonorProtocolsStoredSeparatelyFromSelectedProtocol. selected_protocol is
// the coordinator's negotiated outcome, never a donor claim.
func TestDonorProtocolsStoredSeparatelyFromSelectedProtocol(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	c := fullContract()
	c.Protocols = []string{"fed/v1", "fed/v99"}
	heartbeatWith(t, s, leaf, contractBody(t, c))

	var selected string
	var reported []string
	if err := pool.QueryRow(ctx,
		`SELECT selected_protocol, reported_protocols FROM nodes WHERE id = $1::uuid`, id).
		Scan(&selected, &reported); err != nil {
		t.Fatal(err)
	}
	if selected != wire.ProtocolV1 {
		t.Errorf("selected_protocol = %q; a donor advertisement must never overwrite the "+
			"coordinator's negotiated outcome", selected)
	}
	if !slices.Contains(reported, "fed/v99") {
		t.Errorf("reported_protocols = %v, want the donor's claim stored separately", reported)
	}
}

// TestMalformedHeartbeatBodyRejected. The handler used to discard EVERY decode
// error, so a malformed body was silently treated as an empty one.
func TestMalformedHeartbeatBodyRejected(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	for name, body := range map[string][]byte{
		"malformed": []byte(`{"free_bytes": `),
		"trailing":  []byte(`{"free_bytes":1}{"free_bytes":2}`),
		"wrongtype": []byte(`{"free_bytes":"lots"}`),
	} {
		if w := heartbeatWith(t, s, leaf, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s body = %d, want 400", name, w.Code)
		}
	}
}

// TestEmptyLegacyBodyStillAccepted: a pre-M7.3 donor sends one.
func TestEmptyLegacyBodyStillAccepted(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	if w := heartbeatWith(t, s, leaf, nil); w.Code != http.StatusOK {
		t.Fatalf("empty body = %d (%s), want 200", w.Code, w.Body)
	}
}

// TestReportedFieldsAreLengthAndSyntaxValidated. Untrusted is not unvalidated:
// a donor may claim anything, but not something that makes the census
// unreadable or the row unbounded.
func TestReportedFieldsAreLengthAndSyntaxValidated(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	cases := map[string]func(*wire.RuntimeContract){
		"oversized version":     func(c *wire.RuntimeContract) { c.ClientVersion = strings.Repeat("x", 4096) },
		"newline in digest":     func(c *wire.RuntimeContract) { c.ImageDigest = "sha256:aa\nbb" },
		"too many caps":         func(c *wire.RuntimeContract) { c.Capabilities = make([]string, 100) },
		"empty cap entry":       func(c *wire.RuntimeContract) { c.Capabilities = []string{""} },
		"zero contract version": func(c *wire.RuntimeContract) { c.Version = 0 },
	}
	for name, mutate := range cases {
		c := fullContract()
		mutate(c)
		if w := heartbeatWith(t, s, leaf, contractBody(t, c)); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
}

// TestRuntimeContractIsAtomic: one nested versioned object, so a partial update
// cannot compose a state no donor is actually in.
func TestRuntimeContractIsAtomic(t *testing.T) {
	b, err := json.Marshal(wire.HeartbeatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "runtime_contract") {
		t.Error("an absent contract must be omitted entirely, not sent as an empty object; " +
			"the two silences are what the marker distinguishes")
	}
	for _, forbidden := range []string{`"client_version"`, `"image_digest"`, `"capabilities"`} {
		if strings.Contains(string(b), forbidden) {
			t.Errorf("HeartbeatRequest carries %s at the top level; the contract is sent whole "+
				"or not at all", forbidden)
		}
	}
}
