package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeWidgetDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dist, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("index.html", "<!doctype html><title>nova upload widget</title>")
	must("nova-upload-widget.js", "/* iife */")
	must("assets/uppy-cafebabe.css", ".uppy{}")
	return dist
}

func serve(t *testing.T, h *WidgetStaticHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestNewWidgetStaticNilWhenUnset(t *testing.T) {
	if NewWidgetStatic("") != nil {
		t.Fatal("NewWidgetStatic(\"\") must be nil so /widget/* stays unmounted")
	}
}

func TestWidgetStaticServesDemoIndex(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if got := rr.Body.String(); got == "" || got[:9] != "<!doctype" {
		t.Fatalf("expected demo index.html, got %q", got)
	}
	if csp := rr.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("missing CSP")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("demo index Cache-Control = %q, want no-store", cc)
	}
}

func TestWidgetStaticServesEntryJSNoCache(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/nova-upload-widget.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("entry JS Cache-Control = %q, want no-cache (stable filename)", cc)
	}
}

func TestWidgetStaticHashedAssetImmutable(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/assets/uppy-cafebabe.css")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset Cache-Control = %q", cc)
	}
}

func TestWidgetStaticUnknownPath404NoSPAFallback(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	if rr := serve(t, h, "/widget/does-not-exist.js"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404 (no SPA fallback)", rr.Code)
	}
}

func TestWidgetStaticTraversalBlocked(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	if rr := serve(t, h, "/widget/../../etc/passwd"); rr.Code != http.StatusNotFound {
		t.Fatalf("traversal = %d, want 404", rr.Code)
	}
}
