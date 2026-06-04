package handlers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/masterkey"
)

// MasterKeyAdminHandler serves the M10 master-key rotation endpoints:
//
//	POST /api/v1/admin/keys/rotate-master   (operator)
//	GET  /api/v1/admin/keys/rotation-status (operator)
type MasterKeyAdminHandler struct {
	r     *masterkey.Rotator
	audit *auditlog.Writer // best-effort; nil ⇒ no audit
}

// NewMasterKeyAdminHandler constructs the handler.
func NewMasterKeyAdminHandler(r *masterkey.Rotator, audit *auditlog.Writer) *MasterKeyAdminHandler {
	return &MasterKeyAdminHandler{r: r, audit: audit}
}

type rotateMasterRequest struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
}

// RotateMaster handles POST /api/v1/admin/keys/rotate-master. It validates the
// from/to version pair, starts the background rotation, and returns 202 with
// the initial remaining counts so the caller knows the work ahead.
func (h *MasterKeyAdminHandler) RotateMaster(w http.ResponseWriter, req *http.Request) {
	rid := middleware.RequestIDFromContext(req.Context())
	ctx := req.Context()

	var body rotateMasterRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
		return
	}

	switch err := h.r.Start(ctx, body.FromVersion, body.ToVersion); {
	case err == nil:
		// proceed to 202
	case errors.Is(err, masterkey.ErrToNotActive):
		httputil.WriteError(w, http.StatusBadRequest, "to_not_active", err.Error(), rid)
		return
	case errors.Is(err, masterkey.ErrInvalidFrom):
		httputil.WriteError(w, http.StatusBadRequest, "invalid_from_version", err.Error(), rid)
		return
	case errors.Is(err, masterkey.ErrAlreadyRotating):
		httputil.WriteError(w, http.StatusConflict, "rotation_in_progress", err.Error(), rid)
		return
	default:
		slog.Error("rotate-master", "err", err, "request_id", rid)
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "rotation failed", rid)
		return
	}

	if h.audit != nil {
		h.audit.Write(ctx, auditlog.Entry{
			ActorID:    ownerFromContext(ctx),
			Action:     "master_key.rotation_started",
			TargetType: "master_key_version",
			TargetID:   body.FromVersion,
			Payload:    map[string]any{"from": body.FromVersion, "to": body.ToVersion},
		})
	}

	// Snapshot counts immediately after Start so the 202 body carries the
	// initial total_deks / total_signing_keys (== remaining right after start).
	st, _ := h.r.Status(ctx)
	var totalDEKs, totalSigning int64
	if st.InProgress != nil {
		totalDEKs = st.InProgress.RemainingDEKs
		totalSigning = st.InProgress.RemainingSigningKeys
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"from":               body.FromVersion,
		"to":                 body.ToVersion,
		"total_deks":         totalDEKs,
		"total_signing_keys": totalSigning,
		"status":             st,
	})
}

// RotationStatus handles GET /api/v1/admin/keys/rotation-status. It returns a
// point-in-time snapshot of every master-key version and any in-progress
// rotation. Safe to poll; never mutates state.
func (h *MasterKeyAdminHandler) RotationStatus(w http.ResponseWriter, req *http.Request) {
	rid := middleware.RequestIDFromContext(req.Context())

	st, err := h.r.Status(req.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "status failed", rid)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
