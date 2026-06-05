package handlers

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// adminCSP is the strict Content-Security-Policy for the hermetic admin SPA: no
// third-party origins. style-src allows 'unsafe-inline' only for the CSS-Modules
// runtime style injection; scripts are first-party only (the bundle has no inline
// scripts). It matches nginx/nova.conf.example and the M11 threat-model boundary.
const adminCSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"

// AdminSPAHandler serves the built admin SPA bundle from a directory at /admin/*
// (M11). Vite-hashed assets under /assets/ are immutable-cached; every other path
// falls back to index.html so client-side routing works on deep links. It is
// built only when NOVA_ADMIN_DIST_DIR is set — NewAdminSPA("") returns nil so the
// route is left unmounted (the feature-gate posture). Production may instead serve
// the bundle directly from nginx (M13); this is the self-contained, testable path.
type AdminSPAHandler struct {
	dist  string
	index string
}

// NewAdminSPA returns a handler serving dist, or nil when dist is empty.
func NewAdminSPA(dist string) *AdminSPAHandler {
	if dist == "" {
		return nil
	}
	return &AdminSPAHandler{dist: dist, index: filepath.Join(dist, "index.html")}
}

// Serve resolves the request path to a file under dist (immutable for hashed
// assets), falling back to index.html. The strict CSP + nosniff headers are set
// on every response. Path traversal cannot escape dist: the request path is
// rooted with a leading slash before path.Clean, so any ".." collapses to the
// root and re-joins under dist as a non-existent file (→ SPA fallback).
func (h *AdminSPAHandler) Serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", adminCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")

	rel := strings.TrimPrefix(r.URL.Path, "/admin")
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/")) // leading slash collapses any ".."

	if clean != "/" {
		fsPath := filepath.Join(h.dist, filepath.FromSlash(clean))
		if info, err := os.Stat(fsPath); err == nil && !info.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFile(w, r, fsPath)
			return
		}
	}

	// SPA fallback: index.html for client-routed paths (never cached).
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, h.index)
}
