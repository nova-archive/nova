package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildMigrate builds the runner once per test that needs it.
func buildMigrate(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "migrate")
	build := exec.Command("go", "build", "-o", bin, "./cmd/migrate")
	build.Dir = root
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build: %s", out)
	return bin
}

// TestMigrateDownIsNotExposed (P2-M7.3, D-M7.3-9a).
//
// The normative contract is restore-only rollback — see
// internal/db/migrations/migrations.go — and a shipped `down` subcommand
// contradicts it while inviting exactly the operation the contract forbids.
//
// The refusal is explicit rather than a generic "unknown subcommand", because
// an operator typing `migrate down` at 3am has a rollback in mind and needs to
// be told what going back actually requires, not that they mistyped.
func TestMigrateDownIsNotExposed(t *testing.T) {
	bin := buildMigrate(t)

	cmd := exec.Command(bin, "down")
	cmd.Env = append(os.Environ(), "DATABASE_URL=postgres://nobody@127.0.0.1:1/none")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("`migrate down` must not succeed:\n%s", out)
	}
	body := string(out)
	if !strings.Contains(body, "restore-from-backup") {
		t.Errorf("the refusal does not say what going back requires:\n%s", body)
	}
	if strings.Contains(body, "unknown subcommand") {
		t.Errorf("a removed command deserves a better answer than a typo message:\n%s", body)
	}

	// And it is gone from the documented surface.
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "migrate", "main.go"))
	require.NoError(t, err)
	if strings.Contains(string(src), "goose.DownContext") {
		t.Error("cmd/migrate still calls goose.DownContext")
	}
}

// TestMigrateOnStartRejectsInvalidBoolean. `os.Getenv(x) == "true"` is what
// gets written instead of a parse, and under it NOVA_MIGRATE_ON_START=flase
// silently means false — the operator believes they left auto-apply on and the
// container quietly stops migrating.
func TestMigrateOnStartRejectsInvalidBoolean(t *testing.T) {
	bin := buildMigrate(t)

	cmd := exec.Command(bin, "auto")
	cmd.Env = append(os.Environ(),
		"DATABASE_URL=postgres://nobody@127.0.0.1:1/none",
		"NOVA_MIGRATE_ON_START=flase")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a malformed boolean must be an error:\n%s", out)
	}
	if !strings.Contains(string(out), "NOVA_MIGRATE_ON_START") {
		t.Errorf("the error does not name the variable:\n%s", out)
	}
}

// TestEntrypointUsesTheGuardedApply. The container entrypoint is the one caller
// that runs unattended, so it is the one that must not run `up`.
func TestEntrypointUsesTheGuardedApply(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docker", "init", "entrypoint.sh"))
	require.NoError(t, err)
	body := string(b)

	if !strings.Contains(body, "migrate auto") {
		t.Error("the entrypoint does not use `migrate auto`, which is the only form that " +
			"refuses to apply a destructive range with nobody watching")
	}
	for line := range strings.SplitSeq(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "migrate up") {
			t.Errorf("the entrypoint still runs an unbounded apply: %s", trimmed)
		}
	}
}
