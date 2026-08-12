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
	if _, err := s.q.RegisterNode(ctx, gen.RegisterNodeParams{
		ID:                         pgID,
		NebulaCertFingerprint:      req.NebulaCertFingerprint,
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
	// An evicted node is out of the desired set (its assignments were retired); it
	// must re-register, not heartbeat its way back in (D-M5-5 endpoint matrix). The
	// query reactivates only suspect/unreachable, but rejecting here keeps a stale
	// heartbeat from refreshing an evicted node's last_seen_at.
	if node.Status == gen.NodeStatusEvicted {
		writeError(w, http.StatusForbidden, "registration_required", "evicted node must re-register")
		return
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
