package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/release"
)

// VersionCensusHandler serves the operator's fleet census (P2-M7.3, D-M7.3-13
// and D-M7.3-20).
//
// # Why operator-only
//
// The /admin group guard is RequireRole("operator","moderator") because
// moderators run takedowns. The census is not moderation: it exposes the
// operator's supply-chain expectations, every donor's self-reported build
// identity, and where the two disagree. That follows the key-rotation
// precedent — a nested RequireRole("operator") inside the group.
//
// # Why there is no screen yet
//
// There is no nodes/federation admin surface to extend. P2-M7.6 renders this;
// M7.3 ships the endpoint and the CLI so the data exists to render.
type VersionCensusHandler struct {
	Pool *pgxpool.Pool
	// StaleAfter is the liveness threshold the freshness axis uses.
	StaleAfter time.Duration
	// Now is a test seam.
	Now func() time.Time
}

// NewVersionCensusHandler builds the handler with production defaults.
func NewVersionCensusHandler(pool *pgxpool.Pool, staleAfter time.Duration) *VersionCensusHandler {
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}
	return &VersionCensusHandler{Pool: pool, StaleAfter: staleAfter, Now: time.Now}
}

// censusResponse is the wire shape. It reports the TARGET's own identity
// alongside the fleet, because "supported" is meaningless without saying
// supported by what.
type censusResponse struct {
	Release struct {
		Version      string `json:"version"`
		IntentDigest string `json:"intent_digest"`
		TargetSchema int    `json:"target_schema"`
	} `json:"release"`
	AppliedSchema int64          `json:"applied_schema"`
	SchemaAware   bool           `json:"schema_aware"`
	Nodes         []release.Axes `json:"nodes"`
	OldestVersion string         `json:"oldest_version,omitempty"`
}

// Get returns the census.
func (h *VersionCensusHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rid := middleware.RequestIDFromContext(ctx)

	schema, err := release.AppliedSchema(ctx, h.Pool)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "schema probe failed", rid)
		return
	}
	rows, err := release.ReadCensus(ctx, h.Pool)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "census read failed", rid)
		return
	}

	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	cat := release.Compiled()

	var resp censusResponse
	resp.Release.Version = cat.Version
	resp.Release.IntentDigest = cat.IntentDigest
	resp.Release.TargetSchema = cat.TargetSchema
	resp.AppliedSchema = schema
	resp.SchemaAware = schema >= release.SchemaWithCensusColumns
	resp.Nodes = make([]release.Axes, 0, len(rows))
	for _, row := range rows {
		resp.Nodes = append(resp.Nodes, release.Classify(cat, row, now(), h.StaleAfter))
	}
	resp.OldestVersion = release.OldestByVersion(resp.Nodes)

	// The census is a point-in-time operational view whose whole value is
	// being current; a cached copy would be worse than none.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
