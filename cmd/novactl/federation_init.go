package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

// defaultPKIRoot is the nova-fedpki mount inside the nova-admin container.
const defaultPKIRoot = "/var/lib/nova/fedpki"

func cmdFederationInit(args []string) error {
	fs := flag.NewFlagSet("federation init", flag.ContinueOnError)
	root := fs.String("root", defaultPKIRoot, "PKI volume root (admin-only)")
	cidr := fs.String("overlay-cidr", "", "overlay network CIDR, e.g. 10.42.0.0/24 (required)")
	opIP := fs.String("operator-overlay-ip", "", "coordinator overlay IP, e.g. 10.42.0.1 (required)")
	lhPublic := fs.String("lighthouse-public", "", "publicly reachable lighthouse host:port (required)")
	hostname := fs.String("hostname", "", "coordinator hostname for the server certificate SAN (required)")
	adoptFrom := fs.String("adopt-from", "", "read-only directory holding an existing hand-built PKI to adopt")
	skipPreflight := fs.Bool("skip-preflight", false, "skip /dev/net/tun and route-conflict checks")

	// Destructive. Deliberately not spelled --force: overwriting generated
	// config and destroying a federation CA are not the same act.
	replace := fs.Bool("replace-authority", false,
		"DESTROY and recreate the federation and Nebula CAs (every issued certificate becomes untrusted)")
	destroy := fs.Bool("destroy-existing-federation", false,
		"second acknowledgement, required by --replace-authority when donors are registered")

	if err := parseFlags(fs, args); err != nil {
		return err
	}

	res, err := bootstrap.Init(bootstrap.Params{
		Root:                      *root,
		OverlayCIDR:               *cidr,
		OperatorOverlayIP:         *opIP,
		LighthousePublic:          *lhPublic,
		Hostname:                  *hostname,
		AdoptFrom:                 *adoptFrom,
		SkipPreflight:             *skipPreflight,
		ReplaceAuthority:          *replace,
		DestroyExistingFederation: *destroy,
		RegisteredDonorCount:      registeredDonorCount,
	})
	if err != nil {
		return err
	}

	fmt.Printf("federation ready at %s\n", bootstrap.ActiveDir(*root))
	fmt.Printf("  created:  %d\n", res.Created)
	fmt.Printf("  adopted:  %d\n", res.Adopted)
	fmt.Printf("  existing: %d\n", res.Existing)
	for _, n := range res.Notes {
		fmt.Printf("  note: %s\n", n)
	}
	fmt.Printf("\n  listen_addr:  %s\n", res.Manifest.FederationListenAddr)
	fmt.Printf("  ca:           %s\n", res.Manifest.CAFingerprint)
	fmt.Printf("  lighthouse:   %s\n", res.Manifest.LighthousePublic)
	fmt.Println("\nNext: recreate the coordinator so it reads the new federation block,")
	fmt.Println("then run `federation doctor` before issuing any donor invite.")
	return nil
}

// registeredDonorCount reports how many donors would be orphaned by
// --replace-authority. It is DB-direct and best-effort: when DATABASE_URL is
// absent there is no registry to consult, and the caller treats that as "no
// donors known" rather than blocking a legitimate greenfield replacement.
func registeredDonorCount() (int, error) {
	if os.Getenv("DATABASE_URL") == "" {
		return 0, nil
	}
	var n int
	err := withNodeDB(func(ctx context.Context, q *gen.Queries) error {
		rows, err := q.ListNodes(ctx)
		if err != nil {
			return err
		}
		n = len(rows)
		return nil
	})
	return n, err
}
