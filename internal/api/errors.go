// Package api wires the coordinator's HTTP surface: chi router, middleware,
// and handlers. Handlers translate pkg/coordinator/storage domain errors
// into the JSON Error model from docs/specs/openapi.yaml.
package api

import (
	"encoding/json"
	"net/http"
)

// errorBody is the openapi #/components/schemas/Error shape.
type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// WriteError writes a JSON Error with the given status. Error responses are
// never cacheable.
func WriteError(w http.ResponseWriter, status int, code, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: code, Message: message, RequestID: requestID})
}
