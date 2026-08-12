package coordinator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Safe reactivation of an evicted donor (P2-M7.3, Task 26 step 3).
//
// The trap: the agent loads its durable registration once at boot and never
// re-registers; the coordinator told an evicted node to re-register; and the
// agent reduced that to a warning it never acted on. A supported donor offline
// past the eviction threshold was therefore useless INDEFINITELY, and could not
// be taught otherwise — its binary already shipped.

// evictWithPool moves a registered node to evicted, the way the liveness sweep
// would. `assignment_sync_state` is left 'current' on purpose: the point of the
// reactivation path is that it forces a snapshot, and starting from a state
// that already demands one would prove nothing.
func evictWithPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`UPDATE nodes SET status = 'evicted', assignment_sync_state = 'current' WHERE id = $1`,
		id); err != nil {
		t.Fatal(err)
	}
}

// TestEvictedDonorRecoversWithoutReEnrollment. It returns with its durable
// registration and its existing certificate, and comes back — no new node id,
// no new certificate, no manual state deletion.
func TestEvictedDonorRecoversWithoutReEnrollment(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	var fpBefore string
	if err := pool.QueryRow(ctx,
		`SELECT federation_cert_fingerprint FROM nodes WHERE id = $1`, id).Scan(&fpBefore); err != nil {
		t.Fatal(err)
	}
	evictWithPool(t, ctx, pool, id)

	// The SAME certificate, the same heartbeat the deployed agent already
	// sends. Nothing about the client changes.
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat after eviction = %d (%s); the donor is still stranded",
			w.Code, w.Body)
	}

	var status, sync, fpAfter string
	if err := pool.QueryRow(ctx,
		`SELECT status::text, assignment_sync_state::text, federation_cert_fingerprint
		 FROM nodes WHERE id = $1`, id).Scan(&status, &sync, &fpAfter); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Errorf("status = %q, want active", status)
	}
	if fpAfter != fpBefore {
		t.Error("the certificate changed; that is a re-enrollment, not a recovery")
	}

	// PARTICIPATION, not standing. A full snapshot is forced because the change
	// log this node would diff against has been pruned since it left, and a
	// diff with a missing prefix silently produces a wrong desired set.
	if sync != "snapshot_required" {
		t.Errorf("assignment_sync_state = %q, want snapshot_required — a returning donor must "+
			"reconcile against a snapshot, not a log with a hole in it", sync)
	}
}

// TestReactivationIsRecorded. Undoing an eviction the operator's own liveness
// policy produced is a privileged state change (T1.24). An operator who finds
// a previously evicted donor active again has to be able to find out why.
func TestReactivationIsRecorded(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	evictWithPool(t, ctx, pool, id)

	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", w.Code, w.Body)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'node.reactivate.evicted' AND target_id = $1`,
		id.String()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit_log has %d reactivation entries, want 1", n)
	}
}

// TestReactivationRefusesADifferentCertificate. Eviction is a liveness
// judgement; identity is still identity. A certificate that does not match the
// registered one is somebody else, and a returning-donor path is exactly where
// that would be convenient to overlook.
func TestReactivationRefusesADifferentCertificate(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	registerOK(t, s, caPEM, caKeyPEM, id)
	evictWithPool(t, ctx, pool, id)

	// A NEW certificate for the same node id, signed by the same CA.
	other := issuedClient(t, caPEM, caKeyPEM, id)
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), other))
	if w.Code == http.StatusOK {
		t.Fatal("a certificate that is not the registered one must not reactivate a node")
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM nodes WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "evicted" {
		t.Errorf("status = %q; the refusal still changed the node", status)
	}
}

// TestRevokedIsNotReactivated. Revocation is a TRUST judgement and reactivation
// cannot undo one. The two states are close enough in the schema that
// collapsing them would be an easy mistake with an ugly consequence.
func TestRevokedIsNotReactivated(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	if _, err := pool.Exec(ctx, `UPDATE nodes SET status = 'revoked' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a revoked node", w.Code)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM nodes WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "revoked" {
		t.Errorf("status = %q; a revoked node was reactivated", status)
	}
}

// TestReactivationCanBeDisabled. An operator who wants eviction to be final
// gets the previous behaviour — including the previous trap, which is why the
// default is on.
func TestReactivationCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	evictWithPool(t, ctx, pool, id)

	AllowEvictedReactivation = false
	t.Cleanup(func() { AllowEvictedReactivation = true })

	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when reactivation is disabled", w.Code)
	}
}

// TestNoReplicaIsCreditedOnReturn. The donor's replicas were retired from the
// desired set when it was evicted. They come back by being re-assigned and
// acknowledged, or by a possession audit answering for them — never on a
// month-old machine's say-so, which would let a blob it deleted stay "covered".
func TestNoReplicaIsCreditedOnReturn(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	seedBlob(t, ctx, pool, "reactivation-cid", 5)
	assignViaSeam(t, ctx, pool, "reactivation-cid", id)
	if _, err := pool.Exec(ctx,
		`UPDATE pin_assignments SET state = 'acked' WHERE node_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	// Eviction takes the node's assignments out of the desired set. There is no
	// 'retired' pin_state — the rows go — so model exactly that, then bring the
	// node back and check that nothing reappears.
	if _, err := pool.Exec(ctx,
		`DELETE FROM pin_assignments WHERE node_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	evictWithPool(t, ctx, pool, id)

	w := httptest.NewRecorder()
	s.handleHeartbeat(w, reqWithCert(http.MethodPost, "/fed/v1/heartbeat", []byte(`{}`), leaf))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s)", w.Code, w.Body)
	}

	var assignments int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pin_assignments WHERE node_id = $1`, id).Scan(&assignments); err != nil {
		t.Fatal(err)
	}
	if assignments != 0 {
		t.Errorf("%d assignment(s) reappeared on reactivation; a returning donor gets "+
			"participation, not standing, and its replicas come back only by being "+
			"re-assigned and acknowledged", assignments)
	}
}
