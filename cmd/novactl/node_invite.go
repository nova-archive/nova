package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nova-archive/nova/internal/federation/bootstrap"
)

// cmdNodeInvite is the DOCUMENTED path for onboarding a donor (D-M7.2-4).
// `node issue` and `node nebula-template` survive as expert primitives, but
// only this command produces something a volunteer can run unchanged.
func cmdNodeInvite(args []string) error {
	fs := flag.NewFlagSet("node invite", flag.ContinueOnError)
	root := fs.String("root", defaultPKIRoot, "PKI volume root")
	name := fs.String("name", "", "donor display name (required)")
	nebulaIP := fs.String("nebula-ip", "", "donor overlay IP in CIDR form, e.g. 10.42.0.10/24 (required)")
	image := fs.String("image", "", "digest-pinned nova-node image (required)")
	out := fs.String("out", "", "output directory for the bundle (default ./<name>)")
	nebulaPub := fs.String("nebula-public-key", "",
		"donor-generated Nebula public key to sign; keeps the overlay private key with the donor")
	storageMax := fs.Int64("storage-max-bytes", 0, "donor disk cap in bytes (0 = the 500 GiB default)")
	bandwidth := fs.Int64("bandwidth-budget-bytes-per-day", 0, "donor daily traffic budget (0 = the 50 GiB default)")
	opYAML := fs.String("operator-yaml", defaultOperatorYAML, "operator.yaml, for the pre-issue doctor run")
	skipDoctor := fs.Bool("i-know-what-im-doing", false,
		"issue even when doctor fails; the failing check ids are recorded in the invite manifest")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("node invite: --name is required")
	}
	if *nebulaIP == "" {
		return fmt.Errorf("node invite: --nebula-ip is required")
	}
	outDir := *out
	if outDir == "" {
		outDir = "./" + *name
	}

	// Doctor gates the invite: minting a donor identity against a broken
	// federation produces a bundle that cannot work, and the donor discovers it
	// rather than the operator.
	checks := bootstrap.DoctorLocal(*root, *opYAML)
	if !bootstrap.OK(checks) && !*skipDoctor {
		for _, c := range bootstrap.Failures(checks) {
			fmt.Fprintf(os.Stderr, "FAIL  [%s] %-14s %s\n", c.Plane, c.ID, c.Detail)
		}
		return fmt.Errorf("node invite: doctor reported %d failing check(s); "+
			"fix them, or pass --i-know-what-im-doing to issue anyway",
			len(bootstrap.Failures(checks)))
	}

	res, err := bootstrap.Invite(bootstrap.InviteParams{
		Root: *root, OutDir: outDir, Name: *name,
		NebulaIP: *nebulaIP, NodeImage: *image,
		NebulaPublicKey:            *nebulaPub,
		StorageMaxBytes:            *storageMax,
		BandwidthBudgetBytesPerDay: *bandwidth,
		DoctorStatus:               checks,
	})
	if err != nil {
		return err
	}

	fmt.Printf("invite for %s written to %s\n", *name, res.OutDir)
	fmt.Printf("  node_id:     %s\n", res.NodeID)
	fmt.Printf("  fingerprint: %s\n", res.Fingerprint)
	fmt.Printf("  files:       %d\n", len(res.Files))
	fmt.Println("\nSend the whole directory to the donor. They run `docker compose up -d`")
	fmt.Println("in it, unchanged. It contains none of your CA or coordinator keys.")
	return nil
}
