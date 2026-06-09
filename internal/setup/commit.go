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

// Secrets is the first-run key material. Generated once (GenerateSecrets or the
// /setup/keys/master endpoint), then persisted by Commit — so the master key the
// operator backs up is exactly the one committed.
type Secrets struct {
	MasterKeyHex string // 64 hex chars
	SwarmKey     string // Kubo PSK v1 base16 format
	SigningSeed  string // ed25519 seed hex
}

// GenerateSecrets makes a fresh set of first-run secrets.
func GenerateSecrets() (Secrets, error) {
	mk, err := GenerateMasterKey()
	if err != nil {
		return Secrets{}, err
	}
	swarm, err := GenerateSwarmKey()
	if err != nil {
		return Secrets{}, err
	}
	seed, err := GenerateSigningSeed()
	if err != nil {
		return Secrets{}, err
	}
	return Secrets{MasterKeyHex: mk, SwarmKey: swarm, SigningSeed: seed}, nil
}

// Commit performs the atomic first-run finalize. Ordering is load-bearing:
// secrets -> config -> user -> SENTINEL LAST. A crash before the sentinel
// re-enters setup mode cleanly (every prior step is idempotent/overwrite).
// s must be a fully-populated Secrets (use GenerateSecrets); Commit validates
// that all three fields are non-empty so the caller cannot accidentally commit
// with zero values.
func Commit(ctx context.Context, a Answers, p Paths, s Secrets, uc UserCreator) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if s.MasterKeyHex == "" || s.SwarmKey == "" || s.SigningSeed == "" {
		return fmt.Errorf("setup: commit: secrets incomplete")
	}
	if err := os.MkdirAll(p.SecretsDir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir secrets: %w", err)
	}
	if err := os.MkdirAll(p.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("setup: mkdir config: %w", err)
	}

	// 1. secrets (0600)
	if err := writeSecret(p.SecretsDir, "master-key-v1", s.MasterKeyHex); err != nil {
		return err
	}
	if err := writeSecret(p.SecretsDir, "swarm.key", s.SwarmKey); err != nil {
		return err
	}
	if err := writeSecret(p.SecretsDir, "oidc-signing-key", s.SigningSeed); err != nil {
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
