package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEnvironmentOverride: once a binary is stamped, an environment variable
// that can lie about immutable build information has no legitimate use, and the
// census must not be able to launder a claim through one. The coordinator used
// to let NOVA_VERSION outrank the stamp.
func TestNoEnvironmentOverride(t *testing.T) {
	for _, env := range []string{"NOVA_VERSION", "NOVA_REVISION", "NOVA_BUILD_DATE"} {
		t.Setenv(env, "v9.9.9-forged")
	}
	if Version() == "v9.9.9-forged" {
		t.Error("Version() honoured NOVA_VERSION; build info must not be overridable at runtime")
	}
	if Revision() == "v9.9.9-forged" {
		t.Error("Revision() honoured NOVA_REVISION")
	}
	if BuildDate() == "v9.9.9-forged" {
		t.Error("BuildDate() honoured NOVA_BUILD_DATE")
	}
}

// TestUnstampedDefaultsAreHonest: an unstamped build must say so, and must not
// say it with an empty string — "" is indistinguishable from a field nothing
// ever populated, while "dev"/"unknown" are claims a census can classify.
func TestUnstampedDefaultsAreHonest(t *testing.T) {
	if got := Version(); got != "dev" {
		t.Errorf("Version() = %q, want %q for an unstamped build", got, "dev")
	}
	if got := Revision(); got != "unknown" {
		t.Errorf("Revision() = %q, want %q", got, "unknown")
	}
	if got := BuildDate(); got != "unknown" {
		t.Errorf("BuildDate() = %q, want %q", got, "unknown")
	}
	if Stamped() {
		t.Error("Stamped() = true for an unstamped build")
	}
}

func TestStringCarriesAllThreeFields(t *testing.T) {
	s := String()
	for _, want := range []string{Version(), Revision(), BuildDate()} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}

// TestDockerfilesStampEveryImage. The linker accepts -X for a symbol that does
// not exist and silently discards it, which is exactly how admin.Dockerfile
// came to pass `-X main.buildVersion` — a symbol cmd/novactl never defined —
// and ship a novactl reporting "dev". Building all three images to catch that
// needs BuildKit and minutes; reading them takes milliseconds and catches the
// same class of typo.
func TestDockerfilesStampEveryImage(t *testing.T) {
	const pkgPath = "github.com/nova-archive/nova/internal/buildinfo"
	for _, name := range []string{"coordinator", "node", "admin"} {
		body, err := os.ReadFile(filepath.Join("..", "..", "docker", name+".Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		// Comments explain the old mistake by name, so only instructions count.
		var instructions []string
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				instructions = append(instructions, line)
			}
		}
		text := strings.Join(instructions, "\n")

		if !strings.Contains(text, pkgPath) {
			t.Errorf("%s.Dockerfile does not stamp %s; its binaries will report \"dev\"", name, pkgPath)
		}
		if strings.Contains(text, "main.buildVersion") {
			t.Errorf("%s.Dockerfile still stamps main.buildVersion, a symbol no command defines", name)
		}
		for _, field := range []string{".version=", ".revision=", ".buildDate="} {
			if !strings.Contains(text, field) {
				t.Errorf("%s.Dockerfile does not stamp %s", name, strings.Trim(field, ".="))
			}
		}
		for _, arg := range []string{"NOVA_VERSION", "NOVA_REVISION", "NOVA_BUILD_DATE"} {
			if !strings.Contains(text, "ARG "+arg) {
				t.Errorf("%s.Dockerfile declares no ARG %s", name, arg)
			}
		}
		for _, label := range []string{
			"org.opencontainers.image.version",
			"org.opencontainers.image.revision",
			"org.opencontainers.image.created",
			"org.opencontainers.image.source",
		} {
			if !strings.Contains(text, label) {
				t.Errorf("%s.Dockerfile sets no %s label", name, label)
			}
		}
	}
}

// TestLinkerStampReachesShippedBinaries is the part that matters in production.
// The -X paths in the Makefile and the three Dockerfiles must name symbols that
// exist, and each binary must actually reference this package — Go drops
// packages nothing links, so a package-level test proves nothing about what
// ships. A typo in either place fails silently: the binary keeps reporting
// "dev" and the census keeps being wrong.
//
// Task 8 extends the same --version surface with the release catalog.
func TestLinkerStampReachesShippedBinaries(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	const pkg = "github.com/nova-archive/nova/internal/buildinfo"
	ldflags := "-X " + pkg + ".version=v9.9.9-test" +
		" -X " + pkg + ".revision=cafebabe" +
		" -X " + pkg + ".buildDate=2026-08-11T00:00:00Z"

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	// cmd/coordinator needs cgo/libvips and is covered by the image build; the
	// three pure-Go commands are the ones a unit test can prove.
	for _, cmd := range []string{"node", "novactl", "migrate"} {
		bin := filepath.Join(dir, cmd)
		build := exec.Command("go", "build", "-ldflags", ldflags, "-o", bin, "./cmd/"+cmd)
		build.Dir = root
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("build %s: %v\n%s", cmd, berr, out)
		}
		out, _ := exec.Command(bin, "--version").CombinedOutput()
		got := string(out)
		for _, want := range []string{"v9.9.9-test", "cafebabe", "2026-08-11T00:00:00Z"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s --version = %q, missing %q — check the -X symbol path and that the binary links buildinfo",
					cmd, strings.TrimSpace(got), want)
			}
		}
	}
}
