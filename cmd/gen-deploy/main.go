// Command gen-deploy regenerates the checked-in donor deployment artifacts
// under deploy/donor/ from the canonical templates in internal/deploy.
//
// Before P2-M7.2 deploy/donor/ and the generated invite bundle were parallel
// implementations that had already diverged on ports, topology, paths and
// secrets. They now come from one source, and `make gen-deploy-check` fails
// the build if they drift again.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nova-archive/nova/internal/deploy"
)

// exampleParams are placeholder-but-VALID values for the checked-in example.
// They must load through the production nodeconfig loader, so they are real
// addresses and real budgets rather than REPLACE_ME strings.
func exampleParams() deploy.DonorParams {
	return deploy.DonorParams{
		Name:                 "example",
		NebulaIP:             "10.42.0.10/24",
		LighthouseOverlayIP:  "10.42.0.1",
		LighthousePublicIP:   "203.0.113.7",
		CoordinatorOverlayIP: "10.42.0.1",

		// The checked-in EXAMPLE cannot carry a real digest, because the digest
		// is chosen per release by the operator issuing the invite. This is the
		// one place AllowMutableTags is legitimate; `node invite` never sets it,
		// and the no-mutable-tags gate allowlists these two files by name.
		NodeImage:        "ghcr.io/nova-archive/nova-node:REPLACE-WITH-DIGEST",
		NebulaImage:      deploy.DefaultNebulaImage,
		KuboImage:        deploy.DefaultKuboImage,
		AllowMutableTags: true,

		StorageMaxBytes:            536870912000, // 500 GiB
		BandwidthBudgetBytesPerDay: 53687091200,  // 50 GiB/day
	}
}

const header = `# GENERATED FILE — DO NOT EDIT.
#
# Source: internal/deploy/templates/ (regenerate with ` + "`make gen-deploy`" + `).
# Edits here are overwritten and fail the gen-deploy-check CI gate.
#
# This is a reference copy of the canonical donor deployment. The bundle your
# operator hands you via ` + "`novactl node invite`" + ` is generated from the same
# source with your identities and a pinned image digest filled in.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen-deploy:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	files, err := deploy.RenderDonorBundle(exampleParams())
	if err != nil {
		return err
	}

	out := filepath.Join(root, "deploy", "donor")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	// deploy/donor keeps the historical .example suffix on node.yaml so an
	// operator does not mistake the reference copy for a live config.
	targets := map[string]string{
		"compose.yaml":      "compose.yaml",
		"node.yaml":         "node.yaml.example",
		"nebula-config.yml": "nebula-config.yml.example",
		"kubo-init.sh":      "kubo-init.sh",
	}
	for src, dst := range targets {
		body, ok := files[src]
		if !ok {
			return fmt.Errorf("render produced no %s", src)
		}
		perm := os.FileMode(0o644)
		content := append([]byte(header), body...)
		if strings.HasSuffix(dst, ".sh") {
			perm = 0o755
			// A shebang must stay on line 1, so the header follows it.
			content = body
		}
		if err := os.WriteFile(filepath.Join(out, dst), content, perm); err != nil {
			return err
		}
		fmt.Println("wrote deploy/donor/" + dst)
	}
	return nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate repo root (no go.mod found)")
		}
		dir = parent
	}
}
