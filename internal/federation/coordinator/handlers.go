package coordinator

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// authenticate extracts the verified federation identity from the request's
// peer certificate.
func (s *Server) authenticate(r *http.Request) (transport.Identity, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return transport.Identity{}, errors.New("no peer certificate")
	}
	return transport.IdentityFromCert(r.TLS.PeerCertificates[0])
}

func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// pgUUIDFrom converts a uuid.UUID to pgtype.UUID.
func pgUUIDFrom(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "node_id URI SAN is not a UUID")
		return
	}
	var req wire.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "malformed register body")
		return
	}

	if !slices.Contains(req.SupportedProtocols, wire.ProtocolV1) {
		if s.cfg.OnRegisterFailure != nil {
			s.cfg.OnRegisterFailure("incompatible_protocol")
		}
		writeError(w, http.StatusBadRequest, "incompatible_protocol", "no common fed/v1")
		return
	}
	required := s.cfg.RequiredCapabilities
	if missing, ok := wire.NegotiateCapabilities(req.Capabilities, required); !ok {
		if s.cfg.OnRegisterFailure != nil {
			s.cfg.OnRegisterFailure("missing_capability")
		}
		writeError(w, http.StatusBadRequest, "missing_capability", strings.Join(missing, ","))
		return
	}
	if req.FederationCertFingerprint != "" && req.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusBadRequest, "fingerprint_mismatch", "reported fingerprint != verified cert")
		return
	}

	ctx := r.Context()
	pgID := pgUUIDFrom(nodeUUID)
	existing, err := s.q.GetNodeByID(ctx, pgID)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	if found {
		if existing.Status == gen.NodeStatusRevoked {
			writeError(w, http.StatusForbidden, "node_revoked", "")
			return
		}
		if existing.FederationCertFingerprint != id.Fingerprint {
			writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
			return
		}
	}

	policy := []byte("{}")
	if req.PolicyFilters != nil {
		if b, mErr := json.Marshal(req.PolicyFilters); mErr == nil {
			policy = b
		}
	}
	if required == nil {
		required = []string{}
	}
	caps := req.Capabilities
	if caps == nil {
		caps = []string{}
	}

	// A donor that reports no Nebula certificate fingerprint gets a synthesized
	// one, unique to it (P2-M7.3, found by the mixed-fleet gate).
	//
	// `nodes.nebula_cert_fingerprint` is UNIQUE NOT NULL, and donors before this
	// release sent "". The first such donor took the empty string; every one
	// after it collided on the index. Two pre-release donors cannot both
	// register, and the binaries that do this are already deployed on other
	// people's machines — so the fix has to be here.
	//
	// NOT a nullable column. Postgres would give us the uniqueness semantics for
	// free, but the predecessor coordinator scans this column into a plain
	// string, so a NULL row would break the binary an operator rolls back to —
	// trading a registration bug for a rollback bug.
	//
	// The value says what it is. `unknown:<node-id>` is unique by construction,
	// legible to a human reading the table, and an opaque string to every
	// binary that only ever compares it.
	nebulaFP := req.NebulaCertFingerprint
	if nebulaFP == "" {
		nebulaFP = "unknown:" + id.NodeID
	}
	if _, err := s.q.RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                         pgID,
		NebulaCertFingerprint:      nebulaFP,
		FederationCertFingerprint:  id.Fingerprint,
		DisplayName:                pgText(req.DisplayName),
		GeoDeclared:                pgText(req.GeoDeclared),
		CapacityBytes:              req.CapacityBytes,
		BandwidthBudgetBytesPerDay: req.BandwidthBudgetBytesPerDay,
		PolicyFilters:              policy,
		SelectedProtocol:           pgText(wire.ProtocolV1),
		AdvertisedCapabilities:     caps,
		RequiredCapabilities:       required,
		ClientVersion:              pgText(req.ClientVersion),
		SourceNebulaAddr:           pgText(req.SourceNebulaAddr),
	}); err != nil {
		// A duplicate nebula_cert_fingerprint is not an internal error, and
		// reporting it as one is how this stayed hidden: the column is UNIQUE
		// NOT NULL, donors used to send "" for it, and the SECOND donor ever to
		// register got an opaque 500 while the coordinator logged
		// "register failed" (P2-M7.3, found by the mixed-fleet gate).
		//
		// The donor now sends its certificate's fingerprint, so this is
		// reachable only when two donors genuinely present the same overlay
		// identity — which is worth saying out loud rather than swallowing.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "identity_conflict",
				"another node is already registered with this "+
					constraintSubject(pgErr.ConstraintName)+
					". Two donors cannot share one identity; reissue this donor's bundle.")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "register failed")
		return
	}

	status := http.StatusCreated
	if found {
		status = http.StatusOK
	}
	writeJSON(w, status, wire.RegisterResponse{
		SelectedProtocol:     wire.ProtocolV1,
		RequiredCapabilities: required,
		NodeID:               id.NodeID,
	})
}

// constraintSubject turns a Postgres constraint name into something a
// volunteer's operator can act on.
func constraintSubject(name string) string {
	switch {
	case strings.Contains(name, "nebula_cert_fingerprint"):
		return "Nebula certificate"
	case strings.Contains(name, "federation_cert_fingerprint"):
		return "federation certificate"
	default:
		return "identity (" + name + ")"
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := s.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		return
	}
	nodeUUID, err := uuid.Parse(id.NodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_node_id", "")
		return
	}
	ctx := r.Context()
	node, err := s.q.GetNodeByID(ctx, pgUUIDFrom(nodeUUID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusForbidden, "registration_required", "node must register first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "lookup failed")
		return
	}
	if node.Status == gen.NodeStatusRevoked {
		writeError(w, http.StatusForbidden, "node_revoked", "")
		return
	}
	// An evicted node used to be told to re-register — and could not act on
	// being told (P2-M7.3, Task 26).
	//
	// The deployed agent loads its durable registration once at boot and never
	// repeats it, and it reduces this refusal to a warning it never acts on. So
	// a donor offline past the 30-day threshold heartbeat into a refusal it did
	// not understand, forever: its replicas stranded on a machine the
	// coordinator considered gone, its volunteer looking at a healthy container.
	// The fix cannot live in a binary already deployed on other people's
	// machines, and "delete your state and re-enroll" is the re-enrollment this
	// track exists to prevent.
	//
	// Eviction is a LIVENESS judgement. A donor that returns with its durable
	// registration and a matching certificate has disproved it. Reactivation
	// restores participation and NOT standing: a full snapshot is forced and no
	// replica is credited until it is re-assigned and acknowledged.
	if node.Status == gen.NodeStatusEvicted {
		if rerr := s.ReactivateEvicted(ctx, nodeUUID, node, id.Fingerprint); rerr != nil {
			writeError(w, http.StatusForbidden, "registration_required", rerr.Error())
			return
		}
		refreshed, gerr := s.q.GetNodeByID(ctx, pgUUIDFrom(nodeUUID))
		if gerr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "lookup after reactivation failed")
			return
		}
		node = refreshed
	}
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return
	}

	// P2-M7.3: this used to discard EVERY decode error, not only io.EOF, so a
	// malformed body was silently treated as an empty one and the heartbeat
	// proceeded with zeroed telemetry. An empty body is still accepted — a
	// pre-M7.3 donor sends one — but malformed or trailing JSON is not.
	var req wire.HeartbeatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	if derr := dec.Decode(&req); derr != nil {
		if !errors.Is(derr, io.EOF) {
			writeError(w, http.StatusBadRequest, "bad_request", "malformed heartbeat body")
			return
		}
	} else if derr := dec.Decode(new(json.RawMessage)); !errors.Is(derr, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "trailing content after the heartbeat body")
		return
	}
	if req.RuntimeContract != nil {
		if verr := validateRuntimeContract(req.RuntimeContract); verr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", verr.Error())
			return
		}
	}

	if _, err := s.q.UpdateNodeHeartbeat(ctx, gen.UpdateNodeHeartbeatParams{
		ID:               pgUUIDFrom(nodeUUID),
		LastFreeBytes:    pgInt8(req.FreeBytes),
		LastStoredBytes:  pgInt8(req.StoredBytes),
		SourceNebulaAddr: req.SourceNebulaAddr,
		EgressCapacity:   req.EgressBudgetCapacityBytes,
		EgressRemaining:  req.EgressBudgetRemainingBytes,
		EgressRefill:     req.EgressRefillBytesPerSecond,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "heartbeat failed")
		return
	}

	// The runtime-contract transition runs AFTER the liveness update, so a
	// contract problem never costs a donor its liveness (D-M7.3-7c).
	if err := s.applyRuntimeContract(ctx, nodeUUID, req.RuntimeContract); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "runtime contract failed")
		return
	}

	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "change-log head")
		return
	}
	timers := s.cfg.Timers
	// D-M7.3-21c: nonconformance is advisory, and this is how the VOLUNTEER
	// hears about it. The operator sees the same facts in the census and is the
	// only party that can drain — the heartbeat path never touches draining_at.
	timers.DeprecationMessage = deprecationFor(req.RuntimeContract, time.Now())
	resp := wire.HeartbeatResponse{ConfigUpdates: &timers, CurrentEpoch: head}
	if s.signer != nil {
		resp.RepairTokenPublicKey = s.signer.PublicKeyWire()
	}
	writeJSON(w, http.StatusOK, resp)
}

// pgInt8 wraps a byte count into a (non-null) bigint.
func pgInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }
