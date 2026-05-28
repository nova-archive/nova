// Package api wires the coordinator's HTTP surface: chi router, middleware,
// and handlers. Handlers translate pkg/coordinator/storage domain errors
// into the JSON Error model from docs/specs/openapi.yaml.
package api

import (
	"net/http"

	"github.com/nova-archive/nova/internal/api/httputil"
)

// WriteError writes a JSON Error with the given status. Error responses are
// never cacheable. It delegates to httputil.WriteError so that sub-packages
// (handlers, middleware) can import httputil without creating an import cycle.
func WriteError(w http.ResponseWriter, status int, code, message, requestID string) {
	httputil.WriteError(w, status, code, message, requestID)
}
