package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/wire"
	"github.com/nova-archive/nova/internal/release"
)

// TestDeprecationMessageDeliveredOnHeartbeat. FEDERATION_PROTOCOL.md specified
// config_updates.deprecation_message; nothing carried it, so the specified
// mechanism did not exist.
func TestDeprecationMessageDeliveredOnHeartbeat(t *testing.T) {
	s, _, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// A donor missing a core capability.
	c := fullContract()
	c.Capabilities = []string{wire.CapBlobTransfer} // no pin-change-log, no snapshot
	w := heartbeatWith(t, s, leaf, contractBody(t, c))
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d (%s); nonconformance is advisory and must not fail the "+
			"heartbeat", w.Code, w.Body)
	}
	var resp wire.HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ConfigUpdates == nil || resp.ConfigUpdates.DeprecationMessage == "" {
		t.Fatalf("no deprecation_message for a donor outside the core profile: %+v", resp.ConfigUpdates)
	}
	msg := resp.ConfigUpdates.DeprecationMessage
	for _, want := range []string{wire.CapPinChangeLog, wire.CapSnapshot, "keeps everything it holds"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n  %s", want, msg)
		}
	}
}

// TestConformingDonorGetsNoWarning: a message on every heartbeat for a healthy
// donor would train the volunteer to ignore the channel.
func TestConformingDonorGetsNoWarning(t *testing.T) {
	cat := release.Compiled()
	c := fullContract()
	c.ClientVersion = cat.Version
	c.Capabilities = append(cat.Profiles.Core, wire.CapBlobTransfer)

	if got := deprecationFor(c, time.Now()); got != "" {
		t.Errorf("a current, conforming donor was warned: %q", got)
	}
}

// TestUnsupportedDonorIsToldItIsUntestedNotDead. "Unsupported" means untested.
// A message that reads like an ultimatum invites a volunteer to shut a donor
// down, which is the outcome the fleet policy exists to avoid.
func TestUnsupportedDonorIsToldItIsUntestedNotDead(t *testing.T) {
	cat := windowCatalogForCoordinator()
	past := cat.SupportedPredecessors[2].SupportEpoch.AddDate(2, 0, 0)

	c := fullContract()
	c.ClientVersion = "commit:143c459"
	c.Capabilities = cat.Profiles.Core

	msg := deprecationForWith(cat, c, past)
	if msg == "" {
		t.Fatal("an unsupported donor should hear something")
	}
	for _, want := range []string{"Nothing is evicted", "replicas are kept"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not say %q; unsupported must not read as an ultimatum:\n  %s", want, msg)
		}
	}
}

// TestUnknownVersionIsNotWarnedAbout. Unknown is not incompatible: the interop
// contract is protocol plus capabilities, and a version string was never it.
func TestUnknownVersionIsNotWarnedAbout(t *testing.T) {
	cat := windowCatalogForCoordinator()
	c := fullContract()
	c.ClientVersion = "v9.9.9-nobody-has-heard-of-this"
	c.Capabilities = cat.Profiles.Core

	if got := deprecationForWith(cat, c, time.Now()); got != "" {
		t.Errorf("an unknown version was warned about: %q", got)
	}
}

// TestHeartbeatNeverSetsDrainingAt. draining_at is operator-controlled, and no
// automatic lifecycle transition may set it — the heartbeat least of all, since
// it is the one path a donor can trigger.
func TestHeartbeatNeverSetsDrainingAt(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	// Nonconforming in every way we can express.
	c := fullContract()
	c.Capabilities = []string{}
	c.ClientVersion = "v0.0.1"
	heartbeatWith(t, s, leaf, contractBody(t, c))

	var draining *time.Time
	if err := pool.QueryRow(ctx, `SELECT draining_at FROM nodes WHERE id = $1::uuid`, id).Scan(&draining); err != nil {
		t.Fatal(err)
	}
	if draining != nil {
		t.Fatal("the heartbeat set draining_at; only the operator may, via novactl node drain")
	}

	// And structurally: the heartbeat query must not mention the column at all.
	body, err := os.ReadFile(filepath.Join("..", "..", "db", "queries", "federation.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	start := strings.Index(text, "-- name: UpdateNodeHeartbeat")
	if start < 0 {
		t.Fatal("UpdateNodeHeartbeat not found")
	}
	end := strings.Index(text[start+10:], "\n-- name: ")
	stanza := text[start:]
	if end > 0 {
		stanza = text[start : start+10+end]
	}
	if strings.Contains(stanza, "draining_at") {
		t.Error("UpdateNodeHeartbeat references draining_at; the marker is operator-controlled")
	}
}

// TestNonconformingDonorKeepsExistingReplicas. Losing a capability removes
// eligibility for new work, never data already held.
func TestNonconformingDonorKeepsExistingReplicas(t *testing.T) {
	ctx := context.Background()
	s, pool, caPEM, caKeyPEM := newTestServerPool(t)
	id := uuid.New()
	leaf := registerOK(t, s, caPEM, caKeyPEM, id)

	const cid = "bafy-nonconforming"
	seedBlob(t, ctx, pool, cid, 1024)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AssignPin(ctx, tx, cid, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE pin_assignments SET state='acked' WHERE cid=$1 AND node_id=$2::uuid`, cid, id); err != nil {
		t.Fatal(err)
	}

	// The donor drops every capability.
	c := fullContract()
	c.Capabilities = []string{}
	heartbeatWith(t, s, leaf, contractBody(t, c))

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pin_assignments WHERE cid=$1 AND node_id=$2::uuid AND state='acked'`,
		cid, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("a nonconforming donor lost an acked replica; nonconformance is advisory and " +
			"replacement is the operator's drain, not a heartbeat side effect")
	}
}

// windowCatalogForCoordinator is a target whose predecessor list is, newest
// first: N-1, N-2, then a commit-identified baseline at N-3.
func windowCatalogForCoordinator() release.Catalog {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return release.Catalog{
		Version:      "v0.6.0",
		IntentDigest: "sha256:" + strings.Repeat("0", 64),
		SupportedPredecessors: []release.Predecessor{
			{Kind: "version", Version: "v0.5.0", Schema: 21, SupportEpoch: base.AddDate(0, 10, 0)},
			{Kind: "version", Version: "v0.4.0", Schema: 20, SupportEpoch: base.AddDate(0, 5, 0)},
			{Kind: "commit", Commit: "143c459", Schema: 18, SupportEpoch: base},
		},
		Profiles: release.CapabilityProfiles{Core: []string{wire.CapPinChangeLog, wire.CapSnapshot}},
	}
}
