package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/db/gen"
)

// AuditAdminHandler serves GET /api/v1/admin/audits/integrity
// (openapi listIntegrityAudits): a paginated, filterable view of the
// integrity_audits table. See docs/specs/INTEGRITY_AUDIT.md.
type AuditAdminHandler struct{ q *gen.Queries }

// NewAuditAdminHandler builds the handler over the generated queries.
func NewAuditAdminHandler(q *gen.Queries) *AuditAdminHandler { return &AuditAdminHandler{q: q} }

var validAuditResults = map[string]gen.AuditResult{
	"pass": gen.AuditResultPass,
	"fail": gen.AuditResultFail,
	"skip": gen.AuditResultSkip,
}

var validAuditKinds = map[string]gen.AuditKind{
	"envelope_decode":             gen.AuditKindEnvelopeDecode,
	"key_unwrap":                  gen.AuditKindKeyUnwrap,
	"sample_decrypt":              gen.AuditKindSampleDecrypt,
	"kubo_pin_present":            gen.AuditKindKuboPinPresent,
	"derivative_state_consistent": gen.AuditKindDerivativeStateConsistent,
	"block_hash_valid":            gen.AuditKindBlockHashValid,
	"manifest_consistent":         gen.AuditKindManifestConsistent,
}

// integrityAuditItem is the openapi #/components/schemas/IntegrityAudit shape.
type integrityAuditItem struct {
	ID        int64   `json:"id"`
	CID       string  `json:"cid"`
	AuditKind string  `json:"audit_kind"`
	Result    string  `json:"result"`
	Error     *string `json:"error"`
	AuditedAt string  `json:"audited_at"`
}

// List returns a page of integrity-audit rows, newest first, optionally
// filtered by result and audit_kind.
func (h *AuditAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())

	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}

	var resultFilter gen.NullAuditResult
	if v := r.URL.Query().Get("result"); v != "" {
		res, ok := validAuditResults[v]
		if !ok {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown result filter", rid)
			return
		}
		resultFilter = gen.NullAuditResult{AuditResult: res, Valid: true}
	}

	var kindFilter gen.NullAuditKind
	if v := r.URL.Query().Get("audit_kind"); v != "" {
		k, ok := validAuditKinds[v]
		if !ok {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown audit_kind filter", rid)
			return
		}
		kindFilter = gen.NullAuditKind{AuditKind: k, Valid: true}
	}

	rows, err := h.q.ListIntegrityAudits(r.Context(), gen.ListIntegrityAuditsParams{
		Result:    resultFilter,
		AuditKind: kindFilter,
		Lim:       int32(pg.Limit),
		Off:       int32(pg.Offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list integrity audits", rid)
		return
	}
	total, err := h.q.CountIntegrityAudits(r.Context(), gen.CountIntegrityAuditsParams{
		Result:    resultFilter,
		AuditKind: kindFilter,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count integrity audits", rid)
		return
	}

	data := make([]integrityAuditItem, 0, len(rows))
	for _, row := range rows {
		item := integrityAuditItem{
			ID:        row.ID,
			CID:       row.Cid,
			AuditKind: row.AuditKind,
			Result:    row.Result,
			AuditedAt: row.AuditedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"),
		}
		if row.Error.Valid {
			e := row.Error.String
			item.Error = &e
		}
		data = append(data, item)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"pagination": httputil.Pagination{
			Page:    pg.Page,
			PerPage: pg.PerPage,
			Total:   int(total),
		},
	})
}
