// Package coordinator is the operator-side federation server: the mTLS listener
// (separate from the public/admin mux) serving /fed/v1/register + heartbeat.
// Identity derives from the verified federation client cert. Operator-only —
// never in the cmd/node build graph.
package coordinator

import (
	"encoding/json"
	"net/http"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// Config is the federation server's static configuration.
type Config struct {
	ListenAddr           string
	RequiredCapabilities []string           // [] in M2 — see design § capability negotiation
	Timers               wire.ConfigUpdates // delivered to donors via heartbeat config_updates
	TLS                  TLSMaterial
}

// TLSMaterial holds the PEM bytes for the federation listener.
type TLSMaterial struct {
	CAPEM   []byte
	CertPEM []byte
	KeyPEM  []byte
}

// Server serves the federation control endpoints.
type Server struct {
	q   *gen.Queries
	cfg Config
}

// New constructs a federation Server.
func New(q *gen.Queries, cfg Config) *Server { return &Server{q: q, cfg: cfg} }

// mux returns the federation HTTP routes.
func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/fed/v1/register", s.handleRegister)
	m.HandleFunc("/fed/v1/heartbeat", s.handleHeartbeat)
	return m
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wire.ErrorResponse{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
