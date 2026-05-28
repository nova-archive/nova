// Package httputil contains leaf HTTP helpers shared across the api package
// tree. It imports no internal packages, breaking potential import cycles.
package httputil

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
