package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/tokens"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/orchestrator"
	"github.com/stretchr/testify/require"
)

var repairTargets = orchestrator.ReplicationTargets{Important: 5, Normal: 3, Cache: 2}

// signerServer is a coordinator with a repair signer + positive TTL so the
// /pins/changes and /pins/snapshot handlers actually mint grants.
func signerServer(t *testing.T) (*Server, *pgxpool.Pool, []byte, []byte) {
	t.Helper()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	s.cfg.RepairTokenTTL = time.Hour
	s.cfg.SourceNebulaAddr = "coord:9443"
	signer, err := tokens.NewSignerFromSeed(make([]byte, 32))
	require.NoError(t, err)
	s.SetSourceDeps(signer, nil, time.Now().Add(-time.Minute))
	return s, pool, caPEM, caKeyPEM
}

// makeRepairSource configures node src as an acked holder of cid; repairCap toggles
// the repair-stream/v1 advertisement (off ⇒ read-sourceable only).
func makeRepairSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, src uuid.UUID, cid string, repairCap bool) {
	t.Helper()
	caps := "{read-source/v1,repair-stream/v1}"
	if !repairCap {
		caps = "{read-source/v1}"
	}
	_, err := pool.Exec(ctx, `
		UPDATE nodes SET status='active', assignment_sync_state='current',
			advertised_capabilities=$2::text[], source_nebula_addr='10.0.0.5:9443'
		WHERE id=$1`, pgUUIDFrom(src), caps)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO pin_assignments (cid, node_id, state) VALUES ($1,$2,'acked')`, cid, pgUUIDFrom(src))
	require.NoError(t, err)
}

func TestAssignPinWithSourceRejectsSourceEqualsDest(t *testing.T) {
	ctx := context.Background()
	_, pool, _, _ := newTestServerPool(t)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	same := uuid.New()
	_, err = AssignPinWithSource(ctx, tx, "anycid", same, same, repairTargets)
	require.ErrorIs(t, err, ErrSourceIsDest)
}

func TestAssignPinWithSourceNilStoresSQLNull(t *testing.T) {
	ctx := context.Background()
	_, pool, _, _ := newTestServerPool(t)
	seedBlob(t, ctx, pool, "nilcid", 10)
	dest := seedNode(t, ctx, pool)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "nilcid", dest, uuid.Nil, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var isNull bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source_node_id IS NULL FROM pin_assignments WHERE cid='nilcid' AND node_id=$1`, pgUUIDFrom(dest)).Scan(&isNull))
	require.True(t, isNull, "nil source must store SQL NULL, never the synthetic CoordinatorSourceID")
}

func TestMixedVersionDonorReadSourceableNotRepairSourceable(t *testing.T) {
	ctx := context.Background()
	_, pool, _, _ := newTestServerPool(t)
	seedBlob(t, ctx, pool, "mixcid", 10)
	src := seedNode(t, ctx, pool)
	dest := seedNode(t, ctx, pool)
	makeRepairSource(t, ctx, pool, src, "mixcid", false) // read-source only

	q := gen.New(pool)
	ok, err := q.IsRepairSourceableForCID(ctx, gen.IsRepairSourceableForCIDParams{ID: pgUUIDFrom(src), Cid: "mixcid"})
	require.NoError(t, err)
	require.False(t, ok, "a read-sourceable-only donor is not repair-sourceable")

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = AssignPinWithSource(ctx, tx, "mixcid", dest, src, repairTargets)
	require.ErrorIs(t, err, ErrSourceNotSourceable)
}

func TestPinsChangesNullSourceEmitsCoordinatorSourceID(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := signerServer(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "ccid", 10)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "ccid", id, uuid.Nil, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Changes, 1)
	require.NotNil(t, resp.Changes[0].Source)
	require.Equal(t, wire.CoordinatorSourceID, resp.Changes[0].Source.NodeID, "wire shows synthetic id")

	var isNull bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT source_node_id IS NULL FROM pin_assignments WHERE cid='ccid'`).Scan(&isNull))
	require.True(t, isNull, "DB stores NULL, not the synthetic id")
}

func TestCoordinatorSourceIDEmergencyPath(t *testing.T) {
	// A CID with no donor source (NULL) is served coordinator-as-source — the
	// emergency/local-recoverable path (D-M5-8b) reuses the same NULL-source mint.
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := signerServer(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)
	seedBlob(t, ctx, pool, "emcid", 10)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "emcid", id, uuid.Nil, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Changes, 1)
	require.NotNil(t, resp.Changes[0].Source)
	claims, err := wire.DecodeClaimsUnverified(resp.Changes[0].Source.Token)
	require.NoError(t, err)
	require.Equal(t, wire.CoordinatorSourceID, claims.SourceNodeID, "coordinator-as-source: the coordinator is the source")
	require.Equal(t, id.String(), claims.DestNodeID, "the requesting donor is the dest")
}

func TestPinsChangesDonorSourceMintsBoundGrant(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := signerServer(t)
	dest := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, dest)
	seedBlob(t, ctx, pool, "dcid", 10)
	src := seedNode(t, ctx, pool)
	makeRepairSource(t, ctx, pool, src, "dcid", true)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "dcid", dest, src, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Changes, 1)
	got := resp.Changes[0].Source
	require.NotNil(t, got)
	require.Equal(t, src.String(), got.NodeID, "donor source named on the wire")
	require.Equal(t, "10.0.0.5:9443", got.NebulaAddr, "late-bound to the source's current address")

	claims, err := wire.DecodeClaimsUnverified(got.Token)
	require.NoError(t, err)
	require.Equal(t, src.String(), claims.SourceNodeID)
	require.Equal(t, dest.String(), claims.DestNodeID)
	require.Equal(t, resp.Changes[0].AssignmentID, claims.DestAssignmentID, "Dest* binds THIS pending assignment")
	require.Equal(t, resp.Changes[0].Generation, claims.DestGeneration)
}

func TestLateMintRequeuesWithBackoffWhenSourceNotRepairSourceable(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := signerServer(t)
	dest := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, dest)
	seedBlob(t, ctx, pool, "rqcid", 10)
	src := seedNode(t, ctx, pool)
	makeRepairSource(t, ctx, pool, src, "rqcid", true)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "rqcid", dest, src, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// Source becomes unreachable AFTER the reservation.
	_, err = pool.Exec(ctx, `UPDATE nodes SET status='unreachable' WHERE id=$1`, pgUUIDFrom(src))
	require.NoError(t, err)

	w := httptest.NewRecorder()
	s.handleChanges(w, reqWithCert(http.MethodGet, "/fed/v1/pins/changes?since_seq=0", nil, leaf))
	var resp wire.ChangesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Changes, 1)
	require.Nil(t, resp.Changes[0].Source, "no grant minted for an unsourceable source")

	var attempts int
	var nextSet, isNull bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT source_attempts, source_next_attempt_at IS NOT NULL, source_node_id IS NULL
		 FROM pin_assignments WHERE cid='rqcid' AND node_id=$1`, pgUUIDFrom(dest)).Scan(&attempts, &nextSet, &isNull))
	require.Equal(t, 1, attempts, "attempt counter bumped")
	require.True(t, nextSet, "backoff scheduled")
	require.True(t, isNull, "stored source cleared for re-pick")
}

func TestSnapshotItemCarriesSource(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := signerServer(t)
	dest := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, dest)
	seedBlob(t, ctx, pool, "scid", 10)
	src := seedNode(t, ctx, pool)
	makeRepairSource(t, ctx, pool, src, "scid", true)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = AssignPinWithSource(ctx, tx, "scid", dest, src, repairTargets)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	w := httptest.NewRecorder()
	s.handleSnapshot(w, reqWithCert(http.MethodGet, "/fed/v1/pins/snapshot", nil, leaf))
	var resp wire.SnapshotResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.NotNil(t, resp.Data[0].Source, "snapshot recovery must learn the repair source (D-M5-8a)")
	require.Equal(t, src.String(), resp.Data[0].Source.NodeID)
}
