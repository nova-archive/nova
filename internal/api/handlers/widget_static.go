package handlers

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// buildWidgetCSP builds the strict CSP for the hermetic widget bundle + demo page:
// no third-party origins. blob: in img-src covers Uppy's object-URL previews;
// 'unsafe-inline' in style-src covers the runtime-injected CSS; scripts are
// first-party only. Matches nginx/nova.conf.example.
func buildWidgetCSP() string {
	return "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
}

// WidgetStaticHandler serves the built upload-widget bundle + demo page from a
// directory at /widget/* (M12). Content-hashed assets under /assets/ are
// immutable-cached; the stable entry JS is no-cache; the demo index.html is
// no-store. Unlike the admin SPA there is NO SPA fallback — an unknown path is a
// plain 404 (the widget is a script + demo page, not a routed app). Built only
// when NOVA_WIDGET_DIST_DIR is set — NewWidgetStatic("") returns nil so /widget/*
// is left unmounted (the feature-gate posture). Production may serve the bundle
// directly from nginx (M13); this is the self-contained, testable path.
type WidgetStaticHandler struct {
	dist  string
	index string
	csp   string
}

// NewWidgetStatic returns a handler serving dist, or nil when dist is empty.
func NewWidgetStatic(dist string) *WidgetStaticHandler {
	if dist == "" {
		return nil
	}
	return &WidgetStaticHandler{
		dist:  dist,
		index: filepath.Join(dist, "index.html"),
		csp:   buildWidgetCSP(),
	}
}

// Serve resolves the request path to a file under dist. "/widget" and "/widget/"
// serve the demo index.html; other paths serve the named file or 404 (no SPA
// fallback). Path traversal cannot escape dist: the request path is rooted with a
// leading slash before path.Clean, so any ".." collapses to the root.
func (h *WidgetStaticHandler) Serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", h.csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")

	rel := strings.TrimPrefix(r.URL.Path, "/widget")
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/")) // leading slash collapses any ".."

	if clean == "/" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, h.index)
		return
	}

	fsPath := filepath.Join(h.dist, filepath.FromSlash(clean))
	info, err := os.Stat(fsPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(clean, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, fsPath)
}
