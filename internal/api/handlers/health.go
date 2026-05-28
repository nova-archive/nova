// Package handlers holds the coordinator's HTTP handlers.
package handlers

import (
	"encoding/json"
	"net/http"
	"time"
)

// Health returns a liveness handler. Per openapi.yaml it always returns 200
// when the server is accepting traffic; it does NOT probe DB or Kubo.
func Health(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": version,
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	}
}
