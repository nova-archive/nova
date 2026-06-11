package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/db/gen"
)

// DMCAIntakeHandler serves the public POST /legal/dmca endpoint: it records a
// DMCA takedown notice as a dmca_cases row and returns its id. It takes no
// moderation action — an operator triages the case later via the admin API.
// The route is public; abuse is bounded by the existing rate-limit middleware
// and the 64 KiB body cap below. See
// docs/superpowers/specs/phase1/2026-06-02-phase1-m9-moderation-design.md § HTTP contract.
type DMCAIntakeHandler struct{ q *gen.Queries }

// NewDMCAIntakeHandler builds the handler over the generated queries.
func NewDMCAIntakeHandler(q *gen.Queries) *DMCAIntakeHandler { return &DMCAIntakeHandler{q: q} }

// dmcaIntakeRequest is the JSON body of POST /legal/dmca. All four fields are
// required.
type dmcaIntakeRequest struct {
	ClaimantName   string `json:"claimant_name"`
	ClaimantEmail  string `json:"claimant_email"`
	SwornStatement string `json:"sworn_statement"`
	TargetCID      string `json:"target_cid"`
}

// Submit decodes a DMCA notice, inserts a dmca_cases row, and returns 202 with
// the new case id. No moderation action is taken.
func (h *DMCAIntakeHandler) Submit(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req dmcaIntakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			httputil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large", rid)
			return
		}
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", rid)
		return
	}
	if req.ClaimantName == "" || req.ClaimantEmail == "" || req.SwornStatement == "" || req.TargetCID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request",
			"claimant_name, claimant_email, sworn_statement, and target_cid are required", rid)
		return
	}

	id, err := h.q.InsertDMCACase(r.Context(), gen.InsertDMCACaseParams{
		ClaimantName:   req.ClaimantName,
		ClaimantEmail:  req.ClaimantEmail,
		SwornStatement: req.SwornStatement,
		TargetCid:      req.TargetCID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to record dmca notice", rid)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"case_id": uuid.UUID(id.Bytes).String()})
}
