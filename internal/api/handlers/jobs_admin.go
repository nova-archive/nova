package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/jobs"
)

// JobsAdminHandler serves GET /api/v1/admin/jobs (M11): a read-only, paginated,
// filterable view of the background job queue (stuck / failed / recent work,
// with last_error). Retry is a deliberate M11 non-goal.
type JobsAdminHandler struct{ store *jobs.AdminStore }

// NewJobsAdminHandler builds the handler over the jobs admin store.
func NewJobsAdminHandler(store *jobs.AdminStore) *JobsAdminHandler {
	return &JobsAdminHandler{store: store}
}

var validJobStates = map[string]bool{
	"pending": true, "leased": true, "completed": true, "failed": true, "dead": true,
}

type jobItem struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	State       string  `json:"state"`
	Attempts    int     `json:"attempts"`
	MaxAttempts int     `json:"max_attempts"`
	LastError   *string `json:"last_error"`
	NotBefore   string  `json:"not_before"`
	LeaseUntil  *string `json:"lease_until"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// List returns a page of jobs, newest-first, optionally filtered by state and
// kind.
func (h *JobsAdminHandler) List(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())

	pg, err := httputil.ParsePage(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), rid)
		return
	}

	f := jobs.Filter{Kind: r.URL.Query().Get("kind")}
	if v := r.URL.Query().Get("state"); v != "" {
		if !validJobStates[v] {
			httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown state filter", rid)
			return
		}
		f.State = v
	}

	rows, err := h.store.List(r.Context(), f, pg.Limit, pg.Offset)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to list jobs", rid)
		return
	}
	total, err := h.store.Count(r.Context(), f)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "failed to count jobs", rid)
		return
	}

	data := make([]jobItem, 0, len(rows))
	for _, row := range rows {
		item := jobItem{
			ID:          row.ID.String(),
			Kind:        row.Kind,
			State:       row.State,
			Attempts:    row.Attempts,
			MaxAttempts: row.MaxAttempts,
			NotBefore:   row.NotBefore.UTC().Format(tsLayout),
			CreatedAt:   row.CreatedAt.UTC().Format(tsLayout),
			UpdatedAt:   row.UpdatedAt.UTC().Format(tsLayout),
		}
		if row.LastError.Valid {
			e := row.LastError.String
			item.LastError = &e
		}
		if row.LeaseUntil.Valid {
			v := row.LeaseUntil.Time.UTC().Format(tsLayout)
			item.LeaseUntil = &v
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
