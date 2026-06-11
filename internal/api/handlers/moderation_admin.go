package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/moderation"
)

// ModerationAdminHandler serves the /api/v1/admin/moderation/* surface: the
// five lifecycle actions (quarantine, takedown, clear-legal-hold, restore,
// counter-notice), the decision queue, the DMCA-case views, and the blocklist.
// Operator-vs-moderator authorization is enforced by route mounting (Task 10),
// not here. See
// docs/superpowers/specs/phase1/2026-06-02-phase1-m9-moderation-design.md § HTTP contract.
type ModerationAdminHandler struct {
	svc *moderation.Service
	q   *gen.Queries
}

// NewModerationAdminHandler builds the handler over the moderation service and
// the generated queries.
func NewModerationAdminHandler(svc *moderation.Service, q *gen.Queries) *ModerationAdminHandler {
	return &ModerationAdminHandler{svc: svc, q: q}
}

// mapModErr maps a moderation domain error to (status, code, message). Unknown
// errors collapse to 500 internal so service internals never leak.
func mapModErr(err error) (int, string, string) {
	switch {
	case errors.Is(err, moderation.ErrLegalHold):
		return 409, "legal_hold", "blocked by legal hold"
	case errors.Is(err, moderation.ErrNotQuarantined):
		return 409, "conflict", "blob is not quarantined"
	case errors.Is(err, moderation.ErrBlobNotFound):
		return 404, "not_found", "blob not found"
	default:
		return 500, "internal", "internal server error"
	}
}

// parseTombstoneAfter parses the optional tombstone_after window. It accepts a
// "<n>d" days form (e.g. "14d") in addition to Go durations (e.g. "72h"); an
// empty string defaults to the 14-day counter-notification window.
func parseTombstoneAfter(s string) (time.Duration, error) {
	if s == "" {
		return 14 * 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid tombstone_after %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// decodeJSON reads a JSON body into dst, writing a 400 and returning false on
// failure. Bodies are capped at 64 KiB.
func (h *ModerationAdminHandler) decodeJSON(w http.ResponseWriter, r *http.Request, rid string, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", rid)
		return false
	}
	return true
}

// writeStatus emits the {"status":"<verb>"} body actions return on success.
func writeStatus(w http.ResponseWriter, verb string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": verb})
}

// --- actions -----------------------------------------------------------------

// Quarantine handles POST .../quarantine: block reads for a CID and (optionally)
// place it under legal hold.
func (h *ModerationAdminHandler) Quarantine(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID            string `json:"cid"`
		Rule           string `json:"rule"`
		CaseID         string `json:"case_id"`
		Reason         string `json:"reason"`
		TombstoneAfter string `json:"tombstone_after"`
		LegalHold      bool   `json:"legal_hold"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	after, err := parseTombstoneAfter(req.TombstoneAfter)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}
	if err := h.svc.Quarantine(r.Context(), moderation.QuarantineCmd{
		CID:            req.CID,
		Rule:           req.Rule,
		RuleRef:        req.CaseID,
		Reason:         req.Reason,
		TombstoneAfter: after,
		LegalHold:      req.LegalHold,
		Actor:          ownerFromContext(r.Context()),
	}); err != nil {
		s, c, m := mapModErr(err)
		httputil.WriteError(w, s, c, m, rid)
		return
	}
	writeStatus(w, "quarantined")
}

// Takedown handles POST .../takedown: permanently tombstone (crypto-shred) a CID.
func (h *ModerationAdminHandler) Takedown(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID    string `json:"cid"`
		CaseID string `json:"case_id"`
		Reason string `json:"reason"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	if err := h.svc.Tombstone(r.Context(), moderation.TombstoneCmd{
		CID:     req.CID,
		RuleRef: req.CaseID,
		Reason:  req.Reason,
		Actor:   ownerFromContext(r.Context()),
	}); err != nil {
		s, c, m := mapModErr(err)
		httputil.WriteError(w, s, c, m, rid)
		return
	}
	writeStatus(w, "tombstoned")
}

// ClearLegalHold handles POST .../clear-legal-hold: release a severe-content
// hold so the next sweep can tombstone the CID. (Operator-only via routing.)
func (h *ModerationAdminHandler) ClearLegalHold(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID     string `json:"cid"`
		CaseRef string `json:"case_ref"`
		Reason  string `json:"reason"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	if err := h.svc.ClearLegalHold(r.Context(), req.CID, req.CaseRef, req.Reason, ownerFromContext(r.Context())); err != nil {
		s, c, m := mapModErr(err)
		httputil.WriteError(w, s, c, m, rid)
		return
	}
	writeStatus(w, "legal_hold_cleared")
}

// Restore handles POST .../restore: reverse a quarantine (a tombstone is final).
func (h *ModerationAdminHandler) Restore(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID    string `json:"cid"`
		Reason string `json:"reason"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	if err := h.svc.Restore(r.Context(), req.CID, req.Reason, ownerFromContext(r.Context())); err != nil {
		s, c, m := mapModErr(err)
		httputil.WriteError(w, s, c, m, rid)
		return
	}
	writeStatus(w, "restored")
}

// CounterNotice handles POST .../counter-notice: pause the scheduled tombstone
// while a counter-notification is reviewed.
func (h *ModerationAdminHandler) CounterNotice(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID   string `json:"cid"`
		Notes string `json:"notes"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	if err := h.svc.CounterNotice(r.Context(), req.CID, req.Notes, ownerFromContext(r.Context())); err != nil {
		s, c, m := mapModErr(err)
		httputil.WriteError(w, s, c, m, rid)
		return
	}
	writeStatus(w, "counter_received")
}

// --- listings / reads --------------------------------------------------------

// moderationDecisionItem is one row of the decision queue. decided_by and
// scheduled_tombstone_at are JSON null for system actions / un-scheduled holds.
type moderationDecisionItem struct {
	ID                   string  `json:"id"`
	CID                  string  `json:"cid"`
	Rule                 string  `json:"rule"`
	RuleRef              *string `json:"rule_ref"`
	Action               string  `json:"action"`
	DecidedBy            *string `json:"decided_by"`
	DecidedAt            string  `json:"decided_at"`
	ScheduledTombstoneAt *string `json:"scheduled_tombstone_at"`
	LegalHold            bool    `json:"legal_hold"`
	Notes                *string `json:"notes"`
}

// Queue handles GET .../queue: a paginated, newest-first view of
// moderation_decisions.
func (h *ModerationAdminHandler) Queue(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}
	rows, err := h.q.ListModerationDecisions(r.Context(), gen.ListModerationDecisionsParams{
		Lim: int32(pg.Limit),
		Off: int32(pg.Offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list moderation decisions", rid)
		return
	}
	total, err := h.q.CountModerationDecisions(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count moderation decisions", rid)
		return
	}
	data := make([]moderationDecisionItem, 0, len(rows))
	for _, row := range rows {
		data = append(data, moderationDecisionItem{
			ID:                   uuid.UUID(row.ID.Bytes).String(),
			CID:                  row.Cid,
			Rule:                 row.Rule,
			RuleRef:              textPtr(row.RuleRef),
			Action:               row.Action,
			DecidedBy:            strPtr(row.DecidedBy),
			DecidedAt:            rfc3339(row.DecidedAt),
			ScheduledTombstoneAt: tsPtr(row.ScheduledTombstoneAt),
			LegalHold:            row.LegalHold,
			Notes:                textPtr(row.Notes),
		})
	}
	writePage(w, data, pg, total)
}

// dmcaCaseItem is one row of the DMCA-case listing. actioned_at is JSON null
// while the case is still open.
type dmcaCaseItem struct {
	ID            string  `json:"id"`
	ClaimantName  string  `json:"claimant_name"`
	ClaimantEmail string  `json:"claimant_email"`
	TargetCID     string  `json:"target_cid"`
	ReceivedAt    string  `json:"received_at"`
	ActionedAt    *string `json:"actioned_at"`
	Status        string  `json:"status"`
}

// DMCAList handles GET .../dmca: a paginated, newest-first view of dmca_cases.
func (h *ModerationAdminHandler) DMCAList(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}
	rows, err := h.q.ListDMCACases(r.Context(), gen.ListDMCACasesParams{
		Lim: int32(pg.Limit),
		Off: int32(pg.Offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list dmca cases", rid)
		return
	}
	total, err := h.q.CountDMCACases(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count dmca cases", rid)
		return
	}
	data := make([]dmcaCaseItem, 0, len(rows))
	for _, row := range rows {
		data = append(data, dmcaCaseItem{
			ID:            uuid.UUID(row.ID.Bytes).String(),
			ClaimantName:  row.ClaimantName,
			ClaimantEmail: row.ClaimantEmail,
			TargetCID:     row.TargetCid,
			ReceivedAt:    rfc3339(row.ReceivedAt),
			ActionedAt:    tsPtr(row.ActionedAt),
			Status:        row.Status,
		})
	}
	writePage(w, data, pg, total)
}

// dmcaCaseDetail adds the sworn statement to the listing shape for the single-
// case read.
type dmcaCaseDetail struct {
	dmcaCaseItem
	SwornStatement string `json:"sworn_statement"`
}

// DMCAGet handles GET .../dmca/{id}: the full record for one DMCA case.
func (h *ModerationAdminHandler) DMCAGet(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "id must be a uuid", rid)
		return
	}
	row, err := h.q.GetDMCACase(r.Context(), pgtype.UUID{Bytes: id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.WriteError(w, http.StatusNotFound, "not_found", "dmca case not found", rid)
		return
	}
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to get dmca case", rid)
		return
	}
	out := dmcaCaseDetail{
		dmcaCaseItem: dmcaCaseItem{
			ID:            uuid.UUID(row.ID.Bytes).String(),
			ClaimantName:  row.ClaimantName,
			ClaimantEmail: row.ClaimantEmail,
			TargetCID:     row.TargetCid,
			ReceivedAt:    rfc3339(row.ReceivedAt),
			ActionedAt:    tsPtr(row.ActionedAt),
			Status:        row.Status,
		},
		SwornStatement: row.SwornStatement,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// blocklistItem is one row of the blocklist listing. added_by is JSON null for
// system-added entries.
type blocklistItem struct {
	CID       string  `json:"cid"`
	Reason    string  `json:"reason"`
	Rule      string  `json:"rule"`
	AddedBy   *string `json:"added_by"`
	CreatedAt string  `json:"created_at"`
}

// BlocklistList handles GET .../blocklist: a paginated, newest-first view of the
// blocklist.
func (h *ModerationAdminHandler) BlocklistList(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}
	rows, err := h.q.ListBlocklist(r.Context(), gen.ListBlocklistParams{
		Lim: int32(pg.Limit),
		Off: int32(pg.Offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list blocklist", rid)
		return
	}
	total, err := h.q.CountBlocklist(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count blocklist", rid)
		return
	}
	data := make([]blocklistItem, 0, len(rows))
	for _, row := range rows {
		data = append(data, blocklistItem{
			CID:       row.Cid,
			Reason:    row.Reason,
			Rule:      row.Rule,
			AddedBy:   strPtr(row.AddedBy),
			CreatedAt: rfc3339(row.CreatedAt),
		})
	}
	writePage(w, data, pg, total)
}

// BlocklistAdd handles POST .../blocklist: add a CID to the blocklist with rule
// operator_manual. Idempotent (ON CONFLICT DO NOTHING).
func (h *ModerationAdminHandler) BlocklistAdd(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	var req struct {
		CID    string `json:"cid"`
		Reason string `json:"reason"`
	}
	if !h.decodeJSON(w, r, rid, &req) {
		return
	}
	if req.CID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	var addedBy pgtype.UUID
	if actor := ownerFromContext(r.Context()); actor != nil {
		addedBy = pgtype.UUID{Bytes: *actor, Valid: true}
	}
	if err := h.q.InsertBlocklist(r.Context(), gen.InsertBlocklistParams{
		Cid:     req.CID,
		Reason:  req.Reason,
		Rule:    gen.ModerationRuleOperatorManual,
		AddedBy: addedBy,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to add to blocklist", rid)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "blocked"})
}

// BlocklistRemove handles DELETE .../blocklist/{cid}: remove a CID from the
// blocklist. Idempotent — a missing CID still returns 204.
func (h *ModerationAdminHandler) BlocklistRemove(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	cid := chi.URLParam(r, "cid")
	if cid == "" {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "cid is required", rid)
		return
	}
	if err := h.q.DeleteBlocklist(r.Context(), cid); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to remove from blocklist", rid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- shared rendering helpers ------------------------------------------------

// writePage emits the {"data":[...],"pagination":{...}} envelope all the
// listings share.
func writePage(w http.ResponseWriter, data any, pg httputil.Page, total int64) {
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

// rfc3339 renders a timestamp as RFC3339 in UTC.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// strPtr maps the coalesce'd ” sentinel (a NULL actor/owner) to a JSON null.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// textPtr maps a nullable pgtype.Text to a *string (JSON null when !Valid).
func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// tsPtr maps a nullable pgtype.Timestamptz to an RFC3339 *string (JSON null
// when !Valid).
func tsPtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	s := t.Time.UTC().Format(time.RFC3339)
	return &s
}
