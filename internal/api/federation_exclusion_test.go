package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nova-archive/nova/internal/api"
)

func TestPublicMuxDoesNotServeFederation(t *testing.T) {
	h := api.NewServer(api.ServerConfig{Version: "test"})
	req := httptest.NewRequest(http.MethodPost, "/fed/v1/register", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("/fed/v1/register on public mux = %d, want 404", w.Code)
	}
}
