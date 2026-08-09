package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
)

// D-M7.2-2 step 8: install runtime secrets with explicit permissions, CA keys
// into the admin-only volume ONLY.
//
// This split is the whole custody model. The active PKI directory lives in
// nova-fedpki, which no long-running service mounts — so if the coordinator's
// own certificate stayed there, it could not read it. Runtime identities are
// therefore COPIED out to the paths the daemons actually mount, while the two
// CA private keys never leave nova-fedpki.

// Runtime destinations, relative to the config and secrets volume roots.
const (
	RuntimeFederationDir = "federation" // under the config volume
	RuntimeNebulaDir     = "nebula"     // under the config volume

	RuntimeCoordinatorKey = "nova_coordinator_federation_key"
	RuntimeClientKey      = "nova_coordinator_client_key"
	RuntimeRepairKey      = "nova_repair_signing_key"
	RuntimeLighthouseKey  = "nebula_key"
	RuntimeSwarmKey       = "swarm.key"
)

// runtimeInstall describes one copy from the active PKI to a runtime location.
type runtimeInstall struct {
	from string      // name under <Root>/active
	to   string      // path relative to the destination volume
	perm os.FileMode // 0644 public material, 0600 secrets
}

// publicRuntime is non-secret material for the config volume.
var publicRuntime = []runtimeInstall{
	{FileFederationCACert, filepath.Join(RuntimeFederationDir, FileFederationCACert), perm0644},
	{FileCoordinatorCert, filepath.Join(RuntimeFederationDir, FileCoordinatorCert), perm0644},
	{FileClientCert, filepath.Join(RuntimeFederationDir, FileClientCert), perm0644},
	{FileManifest, filepath.Join(RuntimeFederationDir, FileManifest), perm0644},
	{FileNebulaCACert, filepath.Join(RuntimeNebulaDir, FileNebulaCACert), perm0644},
	{FileLighthouseCert, filepath.Join(RuntimeNebulaDir, FileLighthouseCert), perm0644},
}

// secretRuntime is private material for the secrets volume.
//
// Note what is ABSENT: federation-ca.key and nebula-ca.key. Issuance authority
// stays in nova-fedpki, mounted only by nova-admin.
var secretRuntime = []runtimeInstall{
	{FileCoordinatorKey, RuntimeCoordinatorKey, perm0600},
	{FileClientKey, RuntimeClientKey, perm0600},
	{FileRepairSigningKey, RuntimeRepairKey, perm0600},
	{FileLighthouseKey, RuntimeLighthouseKey, perm0600},
	{FileSwarmKey, RuntimeSwarmKey, perm0600},
}

// InstallRuntime copies runtime identities out of the active PKI directory into
// the config and secrets volumes. It is idempotent and never copies a CA key.
//
// Each write is atomic, so a crash mid-install cannot leave the coordinator
// with a half-written certificate.
func InstallRuntime(root, configDir, secretsDir string) error {
	if configDir == "" && secretsDir == "" {
		return nil
	}
	active := ActiveDir(root)

	if configDir != "" {
		for _, in := range publicRuntime {
			if err := copyRuntime(active, configDir, in); err != nil {
				return err
			}
		}
	}
	if secretsDir != "" {
		for _, in := range secretRuntime {
			if err := copyRuntime(active, secretsDir, in); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyRuntime(active, destRoot string, in runtimeInstall) error {
	src := filepath.Join(active, in.from)
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to install; init did not produce it
		}
		return fmt.Errorf("install runtime: read %s: %w", src, err)
	}
	dst := filepath.Join(destRoot, in.to)
	if err := WriteFileAtomic(dst, b, in.perm); err != nil {
		return fmt.Errorf("install runtime: write %s: %w", dst, err)
	}
	return nil
}

// AssertNoCAKeysInRuntime verifies the custody split held: neither CA private
// key may appear under the runtime volumes. This is the filesystem counterpart
// to the compose-policy gate — one proves the mount is impossible, this proves
// the copy did not happen.
func AssertNoCAKeysInRuntime(configDir, secretsDir string) error {
	for _, dir := range []string{configDir, secretsDir} {
		if dir == "" {
			continue
		}
		for _, name := range []string{FileFederationCAKey, FileNebulaCAKey} {
			hits, err := filepath.Glob(filepath.Join(dir, "**", name))
			if err != nil {
				return err
			}
			direct := filepath.Join(dir, name)
			if _, err := os.Stat(direct); err == nil {
				hits = append(hits, direct)
			}
			if len(hits) > 0 {
				return fmt.Errorf(
					"custody violation: %s is present under the runtime volume %s (%v); "+
						"issuance authority must stay in the admin-only PKI volume", name, dir, hits)
			}
		}
	}
	return nil
}
