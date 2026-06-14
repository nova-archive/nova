package handlers

import (
	"encoding/json"
	"net/http"
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
var fieldEffect = map[string]effect{
	"uploads.cors":                    effectLive,
	"uploads.limits":                  effectLive,
	"uploads.max_upload_size_bytes":   effectLive,
	"uploads.max_concurrent_assembly": effectLive,
	"uploads.session_ttl_seconds":     effectRestart,
	"uploads.tmp_dir":                 effectRestart,
	"uploads.public_uploads":          effectRestart,
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
