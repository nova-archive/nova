package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nova-archive/nova/internal/deploy"
	"github.com/nova-archive/nova/internal/federation/bootstrap"
	"github.com/nova-archive/nova/internal/release"
)

// `novactl node convert-bundle` — v1 donor bundle → v2, by CONVERSION
// (P2-M7.3, D-M7.3-15).
//
// # Why not reissue the invite
//
// Because `node invite` mints a new UUID and new certificates. For an existing
// donor that is a re-enrollment: a new node id, a new federation identity, an
// empty registration, and a fleet in which the volunteer's replicas belong to a
// node that no longer exists. The whole track exists to make an update not look
// like that.
//
// So conversion CONSUMES what is already there — the manifest, the Compose
// file, the identity, the state — and emits only what v2 adds: the donor lock
// and the update script. It touches no certificate, no key, no node id, and no
// volume.
//
// # What v2 is
//
// A bundle whose image refs come from `.env` rather than being baked into the
// Compose file, plus `donor-lock.json` naming the authorized refs and their
// per-component rollback evidence, plus `donor-update.sh` to apply them. That
// is what makes a donor updatable without a new bundle every time.

func cmdNodeConvertBundle(args []string) error {
	fs := flag.NewFlagSet("node convert-bundle", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "the donor bundle directory to convert")
	lockPath := fs.String("lock", "", "release lock file from the verified release bundle")
	intentPath := fs.String("intent", "", "release intent file from the same bundle")
	expectDigest := fs.String("expect-lock-digest", "",
		"the release-lock digest you verified out of band (sha256:...)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	for name, v := range map[string]string{
		"--bundle": *bundleDir, "--lock": *lockPath, "--intent": *intentPath,
		"--expect-lock-digest": *expectDigest,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}

	lock, err := loadVerifiedLock(*lockPath, *intentPath, *expectDigest)
	if err != nil {
		return err
	}

	// The bundle must be one, before anything is written into it.
	manifestPath := filepath.Join(*bundleDir, "invite-manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("node convert-bundle: %w\n"+
			"    --bundle must point at a directory `novactl node invite` produced", err)
	}
	var mf bootstrap.InviteManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return fmt.Errorf("node convert-bundle: %s does not parse: %w", manifestPath, err)
	}
	if mf.NodeID == "" {
		return fmt.Errorf("node convert-bundle: %s names no node; refusing to convert a bundle "+
			"whose identity cannot be read", manifestPath)
	}
	for _, required := range []string{
		"compose.yaml", "node.yaml",
		filepath.Join("federation", "federation.crt"),
		filepath.Join("secrets", "nova_node_federation_key"),
	} {
		if _, err := os.Stat(filepath.Join(*bundleDir, required)); err != nil {
			return fmt.Errorf("node convert-bundle: %s is missing from the bundle. Conversion "+
				"preserves an existing donor; it cannot rebuild one", required)
		}
	}

	// The projection, and the check that it IS the projection this release
	// commits to. Re-deriving and comparing is the verification: there is no
	// second signature, and there does not need to be one.
	dl, err := release.ProjectDonorLock(lock)
	if err != nil {
		return err
	}
	body, err := dl.Render()
	if err != nil {
		return err
	}
	got := release.DonorLockDigestOf(body)
	if lock.DonorLockDigest != "" && got != lock.DonorLockDigest {
		return fmt.Errorf("node convert-bundle: the donor lock this release lock describes is "+
			"%s, but projecting it here produces %s.\n"+
			"    The projection is supposed to be a deterministic function of the release "+
			"lock, so a difference means one of the two documents is not what it claims.",
			lock.DonorLockDigest, got)
	}

	// The update script comes from the canonical templates, so the converted
	// bundle and a freshly issued one carry the same script.
	rendered, err := deploy.RenderDonorBundle(donorParamsFor(mf))
	if err != nil {
		return fmt.Errorf("node convert-bundle: render the update script: %w", err)
	}
	update, ok := rendered["donor-update.sh"]
	if !ok {
		return errors.New("node convert-bundle: the canonical templates produced no donor-update.sh")
	}

	// ONLY the new files. Nothing else in the bundle is rewritten, because
	// everything else is the donor's identity and state.
	if err := writeBundleFile(*bundleDir, "donor-lock.json", body, 0o644); err != nil {
		return err
	}
	if err := writeBundleFile(*bundleDir, "donor-update.sh", update, 0o755); err != nil {
		return err
	}

	// The manifest's version, and nothing else about the manifest.
	mf.Version = bootstrap.BundleSchemaV2
	mf.DonorLockDigest = got
	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	if err := writeBundleFile(*bundleDir, "invite-manifest.json", append(out, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("converted %s to bundle schema %d\n", *bundleDir, bootstrap.BundleSchemaV2)
	fmt.Printf("  release:           %s\n", dl.Release)
	fmt.Printf("  donor lock digest: %s\n", got)
	for _, name := range release.ComponentNames {
		c := dl.Components[name]
		verdict := "rollback UNSAFE"
		if c.Rollback.Safe {
			verdict = "rollback safe → " + c.Rollback.Predecessor
		}
		fmt.Printf("  %-10s %s  (%s)\n", name, c.Ref, verdict)
	}
	fmt.Println()
	fmt.Println("next:")
	fmt.Printf("  1. novactl node rollout authorize --id %s --lock ... --intent ... \\\n", mf.NodeID)
	fmt.Printf("       --expect-lock-digest %s\n", *expectDigest)
	fmt.Println("  2. drain the node")
	fmt.Println("  3. send the volunteer this bundle AND the donor lock digest above;")
	fmt.Println("     they run:  ./donor-update.sh apply --expect-lock-digest <that digest>")
	fmt.Println("  4. confirm the reported digests match, then undrain")
	return nil
}

// donorParamsFor rebuilds the template parameters from the manifest.
//
// Only the fields the UPDATE SCRIPT needs have to be right; the script is not
// per-node. The values below come from the manifest so a future template that
// does interpolate something node-specific gets the real value rather than a
// placeholder that renders and lies.
func donorParamsFor(mf bootstrap.InviteManifest) deploy.DonorParams {
	nebulaIP := mf.NebulaIP
	if nebulaIP == "" {
		nebulaIP = "0.0.0.0/24"
	}
	return deploy.DonorParams{
		Name:                       mf.DisplayName,
		NebulaIP:                   nebulaIP,
		CoordinatorOverlayIP:       mf.CoordinatorURL,
		NodeImage:                  mf.NodeImage,
		NebulaImage:                mf.NebulaImage,
		KuboImage:                  mf.KuboImage,
		BandwidthBudgetBytesPerDay: 1,
		AllowMutableTags:           true, // the refs came from an issued bundle; re-validating them here would refuse a bundle that already exists
	}
}

// writeBundleFile writes atomically. A donor bundle is a thing an operator
// hands to somebody else, and a half-written file in it is a bundle that fails
// on the volunteer's machine rather than on the operator's.
func writeBundleFile(dir, name string, body []byte, perm os.FileMode) error {
	final := filepath.Join(dir, name)
	tmp := final + ".partial"
	if err := os.WriteFile(tmp, body, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
