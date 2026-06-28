package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/nova-archive/nova/internal/api/httputil"
	"github.com/nova-archive/nova/internal/api/middleware"
	"github.com/nova-archive/nova/internal/auditlog"
	"github.com/nova-archive/nova/internal/config"
	"github.com/nova-archive/nova/internal/config/reload"
)

// effect classifies how a config field takes effect when changed.
//
//	live           – hot-applied via the snapshot on the next request
//	restart        – persisted + validated now; applies on coordinator restart
//	envOnlyInert   – persisted + validated, but no runtime consumer reads it yet
type effect string

const (
	effectLive    effect = "live"
	effectRestart effect = "restart"
	effectInert   effect = "env-only-inert"
)

// fieldEffect maps a dotted operator.yaml path (or a "section" prefix) to its
// effect. Longest-prefix match wins; unmatched defaults to restart (safe).
//
// M4.1 coordinator storage/read knobs: all are read once at coordinator
// construction (WithStorageMode, WithCommitGate, NewPruner) and require a
// restart to take effect. The four first-class knobs are listed explicitly so
// the /settings UI can surface them prominently; the remaining advanced tuning
// knobs inherit the "coordinator" prefix (also restart) and appear in the
// advanced section. The effect structure has no formal first-class vs. advanced
// distinction — prominence is conveyed by the presence of an explicit entry.
var fieldEffect = map[string]effect{
	"uploads.cors":                    effectLive,
	"uploads.limits":                  effectLive,
	"uploads.max_upload_size_bytes":   effectLive,
	"uploads.max_concurrent_assembly": effectLive,
	"uploads.session_ttl_seconds":     effectRestart,
	"uploads.tmp_dir":                 effectRestart,
	"uploads.public_uploads":          effectRestart,
	"uploads.default_collection_id":   effectLive,
	"coordinator":                     effectRestart,
	"auth":                            effectRestart,
	"tls":                             effectRestart,
	"operator":                        effectRestart,
	"tos_url":                         effectRestart,
	"moderation":                      effectRestart,
	"source_ip_retention_days":        effectRestart,
	"webhooks":                        effectRestart,
	"orchestrator":                    effectInert,
	"federation":                      effectInert,
	"integrity_audit":                 effectInert,
	"signed_urls":                     effectInert,
	"master_key_rotation":             effectInert,

	// M4.1 first-class storage/read knobs (explicit entries = first-class;
	// advanced tuning knobs fall through the "coordinator" prefix above).
	// All are restart-effect: captured at coordinator construction, no hot reload.
	"coordinator.coordinator_storage_mode":                 effectRestart,
	"coordinator.bounded_cache_max_bytes":                  effectRestart,
	"coordinator.require_replication_quorum_before_commit": effectRestart,
	"coordinator.prune_safety_floor":                       effectRestart,
}

func effectFor(dotted string) effect {
	best, bestLen := effectRestart, -1
	for prefix, e := range fieldEffect {
		if (dotted == prefix || strings.HasPrefix(dotted, prefix+".")) && len(prefix) > bestLen {
			best, bestLen = e, len(prefix)
		}
	}
	return best
}

// ConfigAdminHandler serves the operator-only runtime config API.
type ConfigAdminHandler struct {
	store *reload.Store
	path  string
	audit *auditlog.Writer // optional; set via SetAuditWriter (used by update, next task)
}

// NewConfigAdminHandler constructs a ConfigAdminHandler backed by store; path
// is the operator.yaml file path (used by the write half in the next task).
func NewConfigAdminHandler(store *reload.Store, path string) *ConfigAdminHandler {
	return &ConfigAdminHandler{store: store, path: path}
}

// SetAuditWriter wires in the audit-log writer (used by the PATCH handler in
// the next task; not called in this task).
func (h *ConfigAdminHandler) SetAuditWriter(w *auditlog.Writer) { h.audit = w }

// Get returns the effective config + version + privacy warnings + per-field
// effect/source metadata.
func (h *ConfigAdminHandler) Get(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFromContext(r.Context())
	cfg := h.store.Load()
	m, err := config.ToMap(cfg)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "render config", rid)
		return
	}
	resp := map[string]any{
		"version":          h.store.Version(),
		"config":           m,
		"privacy_warnings": cfg.PrivacyWarnings(),
		"fields":           h.fieldsMeta(m),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// Update handles PATCH /api/v1/admin/config — partial-merge update.
func (h *ConfigAdminHandler) Update(w http.ResponseWriter, r *http.Request) { h.apply(w, r, false) }

// Replace handles PUT /api/v1/admin/config — full-replace update.
func (h *ConfigAdminHandler) Replace(w http.ResponseWriter, r *http.Request) { h.apply(w, r, true) }

// apply is the shared core for Update (PATCH) and Replace (PUT).
// full=false ⇒ JSON Merge Patch semantics; full=true ⇒ full replace.
func (h *ConfigAdminHandler) apply(w http.ResponseWriter, r *http.Request, full bool) {
	rid := middleware.RequestIDFromContext(r.Context())

	// Optimistic concurrency: If-Match absent ⇒ last-writer-wins; present + stale ⇒ 409.
	if m := r.Header.Get("If-Match"); m != "" {
		want, err := strconv.ParseUint(m, 10, 64)
		if err != nil || want != h.store.Version() {
			httputil.WriteError(w, http.StatusConflict, "config_conflict",
				"config version changed; re-read and retry", rid)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body", rid)
		return
	}

	// Re-read on-disk so an out-of-band hand-edit is the merge base.
	base, err := config.LoadFromFile(h.path)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "read config", rid)
		return
	}
	baseMap, err := config.ToMap(base)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "render config", rid)
		return
	}

	mergedMap := patch
	if !full {
		mergedMap = config.MergePatch(baseMap, patch)
	}

	// Validate + preset + defaults via the single load path.
	validated, err := config.FromMap(mergedMap)
	if err != nil {
		httputil.WriteError(w, http.StatusUnprocessableEntity, "config_invalid", err.Error(), rid)
		return
	}

	// Compute effective maps for diff BEFORE persisting (so we can compute
	// restart_required even if WriteAtomic fails for some reason — but we
	// only report it on success).
	newEffMap, err := config.ToMap(validated)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "render config", rid)
		return
	}

	// Persist atomically BEFORE swapping the live snapshot.
	// Nothing persists on validation failure (already returned 422 above).
	if err := config.WriteAtomic(h.path, validated); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "internal", "persist config", rid)
		return
	}

	restart := changedRestartFields(baseMap, newEffMap)
	version := h.store.Swap(validated)
	h.writeAudit(r, baseMap, newEffMap, version, restart)

	live := h.store.Load()
	liveMap, _ := config.ToMap(live)
	resp := map[string]any{
		"version":          version,
		"config":           liveMap,
		"privacy_warnings": live.PrivacyWarnings(),
		"fields":           h.fieldsMeta(liveMap),
		"restart_required": restart,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// changedRestartFields returns sorted dotted leaf paths that changed between
// oldMap and newMap and whose effect is NOT effectLive (i.e. restart required).
func changedRestartFields(oldMap, newMap map[string]any) []string {
	oldLeaves := map[string]string{}
	newLeaves := map[string]string{}
	flattenLeaves("", oldMap, oldLeaves)
	flattenLeaves("", newMap, newLeaves)

	seen := map[string]bool{}
	var restart []string
	consider := func(path string) {
		if seen[path] || effectFor(path) == effectLive {
			return
		}
		seen[path] = true
		restart = append(restart, path)
	}
	for path, nv := range newLeaves {
		if oldLeaves[path] != nv {
			consider(path)
		}
	}
	for path := range oldLeaves {
		if _, ok := newLeaves[path]; !ok {
			consider(path)
		}
	}
	sort.Strings(restart)
	return restart
}

// flattenLeaves recursively walks v (expected to be map[string]any or scalar)
// and populates out with dotted-path → fmt.Sprintf("%v", value) entries.
// Note: slice values are compared by their fmt-stringified form, so a pure
// reorder of a list (e.g. webhooks) can produce a spurious restart_required
// entry — this is advisory-only and safe to ignore if only ordering changed.
func flattenLeaves(prefix string, v any, out map[string]string) {
	if sub, ok := v.(map[string]any); ok {
		for k, vv := range sub {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenLeaves(p, vv, out)
		}
		return
	}
	out[prefix] = fmt.Sprintf("%v", v)
}

// writeAudit emits a best-effort audit log entry for a successful config write.
// It never fails the request — if h.audit is nil or the write errors, it's a no-op.
func (h *ConfigAdminHandler) writeAudit(r *http.Request, oldMap, newMap map[string]any, version uint64, restart []string) {
	if h.audit == nil {
		return
	}
	// Build a minimal diff payload: changed leaf paths with old→new values.
	oldLeaves := map[string]string{}
	newLeaves := map[string]string{}
	flattenLeaves("", oldMap, oldLeaves)
	flattenLeaves("", newMap, newLeaves)
	diff := map[string]any{}
	for path, nv := range newLeaves {
		if oldLeaves[path] != nv {
			diff[path] = map[string]any{"from": oldLeaves[path], "to": nv}
		}
	}
	for path := range oldLeaves {
		if _, ok := newLeaves[path]; !ok {
			diff[path] = map[string]any{"from": oldLeaves[path], "to": nil}
		}
	}
	h.audit.Write(r.Context(), auditlog.Entry{
		ActorID:    ownerFromContext(r.Context()),
		Action:     "config.updated",
		TargetType: "operator_yaml",
		TargetID:   h.path,
		Payload: map[string]any{
			"version":          version,
			"diff":             diff,
			"restart_required": restart,
		},
	})
}

// fieldsMeta walks the leaf dotted paths of m and attaches effect + source.
func (h *ConfigAdminHandler) fieldsMeta(m map[string]any) map[string]any {
	out := map[string]any{}
	pins := h.store.EnvPinned()
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if sub, ok := v.(map[string]any); ok {
			for k, vv := range sub {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, vv)
			}
			return
		}
		meta := map[string]any{"effect": string(effectFor(prefix)), "source": "yaml"}
		if _, pinned := pins[prefix]; pinned {
			meta["source"] = "env"
			meta["shadowed_by_env"] = true
		}
		out[prefix] = meta
	}
	walk("", m)
	return out
}
