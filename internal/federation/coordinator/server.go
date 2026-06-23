// Package coordinator is the operator-side federation server: the mTLS listener
// (separate from the public/admin mux) serving /fed/v1/register + heartbeat.
// Identity derives from the verified federation client cert. Operator-only —
// never in the cmd/node build graph.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/federation/wire"
)

// Config is the federation server's static configuration.
type Config struct {
	ListenAddr           string
	RequiredCapabilities []string           // [] in M2 — see design § capability negotiation
	Timers               wire.ConfigUpdates // delivered to donors via heartbeat config_updates
	TLS                  TLSMaterial
	ChangeLogRetention   time.Duration // default 168h; 0 disables pruning
	PrunePollInterval    time.Duration // default 1h
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

	ln  net.Listener
	srv *http.Server
}

// New constructs a federation Server.
func New(q *gen.Queries, cfg Config) *Server { return &Server{q: q, cfg: cfg} }

// mux returns the federation HTTP routes.
func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/fed/v1/register", s.handleRegister)
	m.HandleFunc("/fed/v1/heartbeat", s.handleHeartbeat)
	m.HandleFunc("GET /fed/v1/pins/changes", s.handleChanges)
	m.HandleFunc("GET /fed/v1/pins/snapshot", s.handleSnapshot)
	m.HandleFunc("POST /fed/v1/pins/{cid}/ack", s.handleAck)
	m.HandleFunc("POST /fed/v1/pins/{cid}/fail", s.handleFail)
	return m
}

// Listen binds the mTLS listener. Call before Run so a bad bind fails fast
// (the coordinator must not declare startup success with a dead federation
// listener — cmd/coordinator binds this BEFORE serving the public mux).
func (s *Server) Listen() error {
	tlsCfg, err := transport.ServerTLSConfig(s.cfg.TLS.CAPEM, s.cfg.TLS.CertPEM, s.cfg.TLS.KeyPEM)
	if err != nil {
		return err
	}
	raw, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.ln = transport.NewTLSListener(raw, tlsCfg)
	s.srv = &http.Server{Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second}
	return nil
}

// Addr returns the bound address (useful with :0). Empty until Listen.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Run serves until ctx is cancelled, then drains. Listen must be called first.
func (s *Server) Run(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("coordinator: federation Listen() not called")
	}
	go s.runRetention(ctx, s.cfg.PrunePollInterval, s.cfg.ChangeLogRetention)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
	}()
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
