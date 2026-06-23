package coordinator

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

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
		writeError(w, http.StatusBadRequest, "incompatible_protocol", "no common fed/v1")
		return
	}
	required := s.cfg.RequiredCapabilities
	if missing, ok := wire.NegotiateCapabilities(req.Capabilities, required); !ok {
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
	if node.FederationCertFingerprint != id.Fingerprint {
		writeError(w, http.StatusForbidden, "fingerprint_mismatch", "presented cert is not the active cert")
		return
	}

	var req wire.HeartbeatRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req) // tolerant: empty body ok

	if _, err := s.q.UpdateNodeHeartbeat(ctx, gen.UpdateNodeHeartbeatParams{
		ID:              pgUUIDFrom(nodeUUID),
		LastFreeBytes:   pgInt8(req.FreeBytes),
		LastStoredBytes: pgInt8(req.StoredBytes),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "heartbeat failed")
		return
	}

	head, err := s.q.GetChangeLogHead(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "change-log head")
		return
	}
	timers := s.cfg.Timers
	resp := wire.HeartbeatResponse{ConfigUpdates: &timers, CurrentEpoch: head}
	if s.signer != nil {
		resp.RepairTokenPublicKey = s.signer.PublicKeyWire()
	}
	writeJSON(w, http.StatusOK, resp)
}

// pgInt8 wraps a byte count into a (non-null) bigint.
func pgInt8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }
