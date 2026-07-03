package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// ListenAndServe binds addr FIRST (so a bind failure is a returned error the
// caller treats as startup-fatal, D-M7-1) and serves until ctx is done.
func ListenAndServe(ctx context.Context, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: bind %s: %w", addr, err)
	}
	return ServeListener(ctx, ln, h)
}

// ServeListener serves an already-bound listener until ctx is done — the
// coordinator binds synchronously at startup and hands the listener here, so
// the bind-failure path stays on the startup call stack.
func ServeListener(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	slog.Info("metrics.listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
