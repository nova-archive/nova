package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/db/gen"
)

// AuditLogAdminHandler serves GET /api/v1/admin/audit-log: a paginated,
// newest-first view of the append-only audit_log, optionally filtered by action
// and target_type. The audit_log is the legal record of every moderation
// decision (operator-only via routing, Task 10). See
// docs/superpowers/specs/phase1/2026-06-02-phase1-m9-moderation-design.md § HTTP contract.
type AuditLogAdminHandler struct{ q *gen.Queries }

// NewAuditLogAdminHandler builds the handler over the generated queries.
func NewAuditLogAdminHandler(q *gen.Queries) *AuditLogAdminHandler {
	return &AuditLogAdminHandler{q: q}
}

// auditLogItem is one audit_log row. actor_id is JSON null for system actions
// (the scheduled-tombstone sweep records actor_id=NULL). payload is the raw
// stored JSON document.
type auditLogItem struct {
	ID         int64           `json:"id"`
	ActorID    *string         `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Payload    json.RawMessage `json:"payload"`
	At         string          `json:"at"`
}

// List returns a page of audit_log rows, newest first, optionally filtered by
// action and/or target_type.
func (h *AuditLogAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())

	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}

	var actionFilter pgtype.Text
	if v := r.URL.Query().Get("action"); v != "" {
		actionFilter = pgtype.Text{String: v, Valid: true}
	}
	var targetTypeFilter pgtype.Text
	if v := r.URL.Query().Get("target_type"); v != "" {
		targetTypeFilter = pgtype.Text{String: v, Valid: true}
	}

	rows, err := h.q.ListAuditLog(r.Context(), gen.ListAuditLogParams{
		Action:     actionFilter,
		TargetType: targetTypeFilter,
		Lim:        int32(pg.Limit),
		Off:        int32(pg.Offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list audit log", rid)
		return
	}
	total, err := h.q.CountAuditLog(r.Context(), gen.CountAuditLogParams{
		Action:     actionFilter,
		TargetType: targetTypeFilter,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count audit log", rid)
		return
	}

	data := make([]auditLogItem, 0, len(rows))
	for _, row := range rows {
		item := auditLogItem{
			ID:         row.ID,
			ActorID:    strPtr(row.ActorID),
			Action:     row.Action,
			TargetType: row.TargetType,
			TargetID:   row.TargetID,
			Payload:    json.RawMessage(row.Payload),
			At:         rfc3339(row.At),
		}
		if len(item.Payload) == 0 {
			item.Payload = json.RawMessage("null")
		}
		data = append(data, item)
	}

	writePage(w, data, pg, total)
}
