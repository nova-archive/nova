package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

const (
	defaultOperatorYAML = "/etc/nova/operator.yaml"
	defaultReadyzURL    = "http://127.0.0.1:2112/readyz"
)

// cmdFederationDoctor runs Plane A always, and Plane B when --live is set.
//
// The two planes are separate because they run in DIFFERENT containers:
// Plane A in nova-admin over its own mounts, Plane B in nova-doctor, which
// shares the coordinator's network namespace. Neither needs the Docker socket.
// Plane C is `federation compose-policy`.
func cmdFederationDoctor(args []string) error {
	fs := flag.NewFlagSet("federation doctor", flag.ContinueOnError)
	root := fs.String("root", defaultPKIRoot, "PKI volume root")
	opYAML := fs.String("operator-yaml", defaultOperatorYAML, "path to operator.yaml")
	live := fs.Bool("live", false, "also run the live overlay checks (requires the coordinator network namespace)")
	readyz := fs.String("readyz-url", defaultReadyzURL, "coordinator diagnostic endpoint")
	fedAddr := fs.String("federation-addr", "", "overlay address to probe with mTLS (default: from the manifest)")
	asJSON := fs.Bool("json", false, "emit machine-readable results")
	timeout := fs.Duration("timeout", 5*time.Second, "per-probe timeout")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	checks := bootstrap.DoctorLocal(*root, *opYAML)

	if *live {
		active := bootstrap.ActiveDir(*root)
		opts := bootstrap.LiveOpts{
			ReadyzURL:      *readyz,
			FederationAddr: *fedAddr,
			Timeout:        *timeout,
		}
		// nova-doctor deliberately holds NO CA key: only the CA certificate and
		// the coordinator CLIENT identity, which is all an mTLS probe needs.
		opts.CAPEM, _ = os.ReadFile(filepath.Join(active, bootstrap.FileFederationCACert))
		opts.ClientCertPEM, _ = os.ReadFile(filepath.Join(active, bootstrap.FileClientCert))
		opts.ClientKeyPEM, _ = os.ReadFile(filepath.Join(active, bootstrap.FileClientKey))

		if opts.FederationAddr == "" {
			if mf, err := bootstrap.ReadManifest(active); err == nil {
				opts.FederationAddr = mf.FederationListenAddr
			}
		}
		checks = append(checks, bootstrap.DoctorLive(context.Background(), opts)...)
	}

	if *asJSON {
		if err := bootstrap.EmitJSON(os.Stdout, checks); err != nil {
			return err
		}
	} else {
		for _, c := range checks {
			mark := "ok  "
			if c.Status == "fail" {
				mark = "FAIL"
			} else if c.Status == "skip" {
				mark = "skip"
			}
			fmt.Printf("%s  [%s] %-14s %s\n", mark, c.Plane, c.ID, c.Detail)
		}
	}

	if !bootstrap.OK(checks) {
		return fmt.Errorf("federation doctor: %d check(s) failed", len(bootstrap.Failures(checks)))
	}
	if !*asJSON {
		fmt.Println("\nfederation doctor: all checks passed")
	}
	return nil
}
