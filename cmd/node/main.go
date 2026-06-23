// Command nova-node is the Nova donor pinning node. In P2-M1 it loads and
// validates node.yaml, serves a loopback health endpoint, and runs a no-op
// agent loop — NO live federation. Live registration/transport arrive in M2+.
//
// Flags:
//
//	--config PATH    node.yaml path (required)
//	--validate       load + validate, then exit (0 ok / non-zero on error)
//	--healthcheck    GET the configured health endpoint, then exit (container HEALTHCHECK)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nova-archive/nova/internal/federation/transport"
	"github.com/nova-archive/nova/internal/node/agent"
	nodeconfig "github.com/nova-archive/nova/internal/node/config"
	"github.com/nova-archive/nova/internal/node/ipfsclient"
	"github.com/nova-archive/nova/internal/node/state"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "nova-node:", err)
		os.Exit(1)
	}
}

// newFlagSet builds a ContinueOnError flag set writing usage to w.
func newFlagSet(w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("nova-node", flag.ContinueOnError)
	fs.SetOutput(w)
	return fs
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet(stderr)
	var (
		configPath  = fs.String("config", "", "path to node.yaml")
		validate    = fs.Bool("validate", false, "validate config and exit")
		healthcheck = fs.Bool("healthcheck", false, "probe the health endpoint and exit")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("--config is required")
	}
	cfg, err := nodeconfig.LoadFromFile(*configPath)
	if err != nil {
		return err
	}
	switch {
	case *validate:
		fmt.Fprintln(stdout, "nova-node: config OK")
		return nil
	case *healthcheck:
		return probeHealth(cfg.HealthListenAddr)
	default:
		return serve(cfg, stdout)
	}
}

func probeHealth(addr string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func serve(cfg *nodeconfig.Config, stdout io.Writer) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","mode":"node-skeleton"}`)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind synchronously so a bad/occupied address fails fast instead of being
	// swallowed in a goroutine while the process blocks forever.
	ln, err := net.Listen("tcp", cfg.HealthListenAddr)
	if err != nil {
		return fmt.Errorf("health listen %s: %w", cfg.HealthListenAddr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srvErr := make(chan error, 1)
	go func() {
		if e := srv.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
			srvErr <- e
		}
	}()
	fmt.Fprintf(stdout, "nova-node: health on %s\n", cfg.HealthListenAddr)

	caPEM, err := os.ReadFile(cfg.FederationCAPath)
	if err != nil {
		return fmt.Errorf("read federation ca: %w", err)
	}
	certPEM, err := os.ReadFile(cfg.FederationCertPath)
	if err != nil {
		return fmt.Errorf("read federation cert: %w", err)
	}
	keyPEM, err := os.ReadFile(cfg.FederationKeyPath)
	if err != nil {
		return fmt.Errorf("read federation key: %w", err)
	}
	tlsCfg, err := transport.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("federation client tls: %w", err)
	}
	client := agent.NewHTTPClient(cfg.CoordinatorURL, tlsCfg)
	regStore := state.NewFileRegistrationStore(cfg.StorageDir)
	cursor := state.NewFileStore(cfg.StorageDir)
	assignments := state.NewFileAssignmentStore(cfg.StorageDir)
	pinner := ipfsclient.New(cfg.KuboAPIAddr)
	progress, err := state.NewFileProgressStore(filepath.Join(cfg.StorageDir, "state"))
	if err != nil {
		return fmt.Errorf("progress store: %w", err)
	}
	ag := agent.New(cfg, regStore, cursor, assignments, client,
		time.Duration(cfg.HeartbeatIntervalSeconds())*time.Second,
		time.Duration(cfg.PinsPollIntervalSeconds())*time.Second)
	ag = agent.WithSource(ag, client, pinner, progress, cfg.StorageMaxBytes)
	go func() {
		if e := ag.Run(ctx); e != nil && ctx.Err() == nil {
			slog.Error("nova-node agent stopped", "err", e)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done(): // SIGINT/SIGTERM
	case runErr = <-srvErr: // health server failed after bind
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if sErr := srv.Shutdown(shutCtx); sErr != nil && runErr == nil {
		runErr = sErr
	}
	return runErr
}
