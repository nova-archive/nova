package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Paths are the on-disk locations the commit writes to (the config + secrets volumes).
type Paths struct {
	ConfigDir  string // operator.yaml, nova.conf, tls/, .bootstrap-complete
	SecretsDir string // master-key-v1, swarm.key, oidc-signing-key (0600)
	Sentinel   string // typically ConfigDir/.bootstrap-complete
}

// UserCreator inserts the first operator account. Implemented over gen.Queries
// in production (commit_db.go); faked in unit tests.
type UserCreator interface {
	CreateOperator(ctx context.Context, email, plain string) error
}

// Commit performs the atomic first-run finalize. Ordering is load-bearing:
// secrets -> config -> user -> SENTINEL LAST. A crash before the sentinel
// re-enters setup mode cleanly (every prior step is idempotent/overwrite).
func Commit(ctx context.Context, a Answers, p Paths, uc UserCreator) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.SecretsDir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir secrets: %w", err)
	}
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir config: %w", err)
	}

	// 1. secrets (0600)
	mk, err := GenerateMasterKey()
	if err != nil {
		return err
	}
	swarm, err := GenerateSwarmKey()
	if err != nil {
		return err
	}
	seed, err := GenerateSigningSeed()
	if err != nil {
		return err
	}
	if err := writeSecret(p.SecretsDir, "master-key-v1", mk); err != nil {
		return err
	}
	if err := writeSecret(p.SecretsDir, "swarm.key", swarm); err != nil {
		return err
	}
	if err := writeSecret(p.SecretsDir, "oidc-signing-key", seed); err != nil {
		return err
	}

	// 2. config (operator.yaml + nova.conf + tls/)
	if _, err := ProvisionTLS(a, filepath.Join(p.ConfigDir, "tls")); err != nil {
		return err
	}
	oy, err := RenderOperatorYAML(a)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "operator.yaml"), oy, 0o644); err != nil {
		return fmt.Errorf("setup: write operator.yaml: %w", err)
	}
	nc, err := RenderNginx(a)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.ConfigDir, "nova.conf"), []byte(nc), 0o644); err != nil {
		return fmt.Errorf("setup: write nova.conf: %w", err)
	}

	// 3. operator user (plain password passed to UserCreator; DB impl hashes it)
	if err := uc.CreateOperator(ctx, a.AdminEmail, a.AdminPassword); err != nil {
		return fmt.Errorf("setup: create operator: %w", err)
	}

	// 4. sentinel LAST (atomic single write).
	if err := os.WriteFile(p.Sentinel, []byte("bootstrap-complete schema=1\n"), 0o644); err != nil {
		return fmt.Errorf("setup: write sentinel: %w", err)
	}
	return nil
}

func writeSecret(dir, name, contents string) error {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
		return fmt.Errorf("setup: write secret %s: %w", name, err)
	}
	return nil
}
