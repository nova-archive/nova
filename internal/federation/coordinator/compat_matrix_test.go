package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// D-M7-3: "required" is the CONFIGURED ServerConfig.RequiredCapabilities set.
// The canonical M7 required profile is {pin-change-log/v1, snapshot/v1}; the
// four route-gated capabilities must NOT appear in it (a capability cannot be
// both — required rejects at register, route-gated degrades per feature).
//
// P2-M7.3 D-M7.3-22: these are now the SHARED profiles cmd/coordinator wires,
// not locally declared copies. The disjointness assertion below used to be
// evaluated against a profile this file built itself, so it passed while
// production required blob-transfer/v1 and contradicted it.
var requiredProfile = ProductionRequiredCapabilities
var routeGated = RouteGatedCapabilities

func registerWith(t *testing.T, s *Server, caPEM, caKeyPEM []byte, protocols, caps []string) *httptest.ResponseRecorder {
	t.Helper()
	leaf := issuedClient(t, caPEM, caKeyPEM, uuid.New())
	body, _ := json.Marshal(wire.RegisterRequest{
		SupportedProtocols:        protocols,
		Capabilities:              caps,
		FederationCertFingerprint: transport.FingerprintDER(leaf),
		CapacityBytes:             1 << 30,
	})
	w := httptest.NewRecorder()
	s.handleRegister(w, reqWithCert(http.MethodPost, "/fed/v1/register", body, leaf))
	return w
}

func TestRequiredCapabilityRejectsRegistration(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	s.cfg.RequiredCapabilities = requiredProfile

	// Advertising only ONE of the two required capabilities fails closed with
	// the missing one named.
	w := registerWith(t, s, caPEM, caKeyPEM, []string{wire.ProtocolV1}, []string{wire.CapPinChangeLog})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var e wire.ErrorResponse
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "missing_capability" || !strings.Contains(e.Message, wire.CapSnapshot) {
		t.Fatalf("error = %+v, want missing_capability naming %s", e, wire.CapSnapshot)
	}

	// No common fed/v1 protocol fails before capabilities are even considered.
	w2 := registerWith(t, s, caPEM, caKeyPEM, []string{"fed/v2"}, requiredProfile)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w2.Code)
	}
	json.Unmarshal(w2.Body.Bytes(), &e)
	if e.Code != "incompatible_protocol" {
		t.Fatalf("code = %q, want incompatible_protocol", e.Code)
	}
}

func TestRouteGatedCapabilitiesRegisterFine(t *testing.T) {
	s, caPEM, caKeyPEM := newTestServer(t)
	s.cfg.RequiredCapabilities = requiredProfile

	// A donor advertising ONLY the required profile — none of blob-transfer/
	// read-source/repair-stream/audit-block-hash — registers fine. This PROVES
	// the four are route-gated, not required (D-M7-3).
	w := registerWith(t, s, caPEM, caKeyPEM, []string{wire.ProtocolV1}, requiredProfile)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body)
	}
}

// seedCapNode seeds an active/current acked holder of cid with exactly caps.
func seedCapNode(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cid string, caps []string) pgtype.UUID {
	t.Helper()
	id := uuid.New()
	capArr := "{" + strings.Join(caps, ",") + "}"
	_, err := pool.Exec(ctx, `
		INSERT INTO nodes (id, nebula_cert_fingerprint, federation_cert_fingerprint, capacity_bytes,
		                   bandwidth_budget_bytes_per_day, policy_filters, status, assignment_sync_state,
		                   trust_state, advertised_capabilities, effective_capabilities, source_nebula_addr, last_seen_at,
		                   last_stored_bytes)
		VALUES ($1::uuid, $2, $3, 1073741824, 1073741824, '{}', 'active', 'current',
		        'trusted', $4::text[], $4::text[], $5, now(), 1000000)`,
		id.String(), id.String()+"-nfp", id.String()+"-ffp", capArr, "10.42.0.9:9443")
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1, $2::uuid, 'acked')`, cid, id)
	if err != nil {
		t.Fatal(err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func TestRouteGatedSelectionSkips(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t, ctx)
	q := gen.New(pool)
	_, err := pool.Exec(ctx, `
		INSERT INTO blobs (cid, mime_type, byte_size, state, product, envelope_version)
		VALUES ('cap-cid', 'image/jpeg', 1000, 'active', 'image', 2)`)
	if err != nil {
		t.Fatal(err)
	}
	// n1: holder WITHOUT any route-gated capability. n2: full capability set.
	n1 := seedCapNode(t, ctx, pool, "cap-cid", []string{wire.CapPinChangeLog, wire.CapSnapshot})
	n2 := seedCapNode(t, ctx, pool, "cap-cid", []string{
		wire.CapPinChangeLog, wire.CapSnapshot,
		wire.CapReadSource, wire.CapRepairStream, wire.CapAuditBlockHash,
	})

	// read-source/v1 gates read selection AND the safety count.
	holders, err := q.ListSourceableHolders(ctx, gen.ListSourceableHoldersParams{Cid: "cap-cid", StaleSecs: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].NodeID != n2 {
		t.Fatalf("sourceable holders = %v, want only the read-source-capable n2", holders)
	}
	count, err := q.CountSourceableHolders(ctx, gen.CountSourceableHoldersParams{Cid: "cap-cid", StaleSecs: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("CountSourceableHolders = %d, want 1 (n1 lacks read-source/v1)", count)
	}

	// repair-stream/v1 gates repair-source selection + the reservation guard.
	src, err := q.ListRepairSourceHolders(ctx, gen.ListRepairSourceHoldersParams{
		Cid: "cap-cid", Size: pgtype.Int8{Int64: 100, Valid: true},
	})
	if err == pgx.ErrNoRows {
		t.Fatal("expected the repair-capable n2 to be selected")
	}
	if err != nil {
		t.Fatal(err)
	}
	if src.NodeID != n2 {
		t.Fatalf("repair source = %v, want n2", src.NodeID)
	}
	ok1, err := q.IsRepairSourceableForCID(ctx, gen.IsRepairSourceableForCIDParams{ID: n1, Cid: "cap-cid"})
	if err != nil || ok1 {
		t.Fatalf("n1 repair-sourceable = %v (err %v), want false", ok1, err)
	}
	ok2, err := q.IsRepairSourceableForCID(ctx, gen.IsRepairSourceableForCIDParams{ID: n2, Cid: "cap-cid"})
	if err != nil || !ok2 {
		t.Fatalf("n2 repair-sourceable = %v (err %v), want true", ok2, err)
	}

	// audit-block-hash/v1 gates audit scheduling.
	due, err := q.SelectDueAuditNodes(ctx, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.NodeID == n1 {
			t.Fatal("n1 lacks audit-block-hash/v1 and must not be audit-scheduled")
		}
	}
	found := false
	for _, d := range due {
		if d.NodeID == n2 {
			found = true
		}
	}
	if !found {
		t.Fatal("audit-capable n2 must be audit-scheduled")
	}
}

func TestCanonicalRequiredProfileDisjointFromRouteGated(t *testing.T) {
	// Guard: keeps the spec's classification honest if someone edits either
	// profile later — a capability cannot be both required and route-gated.
	set := map[string]bool{}
	for _, c := range requiredProfile {
		set[c] = true
	}
	for _, c := range routeGated {
		if set[c] {
			t.Fatalf("capability %s is in BOTH the required profile and the route-gated set", c)
		}
	}
	// And both stay within the wire-known capability constants.
	known := map[string]bool{
		wire.CapPinChangeLog: true, wire.CapSnapshot: true, wire.CapBlobTransfer: true,
		wire.CapReadSource: true, wire.CapRepairStream: true, wire.CapAuditBlockHash: true,
	}
	for _, c := range append(append([]string{}, requiredProfile...), routeGated...) {
		if !known[c] {
			t.Fatalf("capability %s is not a wire-known constant", c)
		}
	}
}
