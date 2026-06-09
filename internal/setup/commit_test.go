package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeUserCreator struct {
	called bool
}

func (f *fakeUserCreator) CreateOperator(_ context.Context, email, passwordHash string) error {
	f.called = true
	return nil
}

func tmpPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	return Paths{
		ConfigDir:  filepath.Join(root, "config"),
		SecretsDir: filepath.Join(root, "secrets"),
		Sentinel:   filepath.Join(root, "config", ".bootstrap-complete"),
	}
}

func TestCommit_SentinelWrittenLast_AndModes(t *testing.T) {
	p := tmpPaths(t)
	uc := &fakeUserCreator{}
	a := validAnswers()
	if err := Commit(context.Background(), a, p, uc); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	for _, name := range []string{"master-key-v1", "swarm.key", "oidc-signing-key"} {
		fi, err := os.Stat(filepath.Join(p.SecretsDir, name))
		if err != nil {
			t.Fatalf("missing secret %s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, fi.Mode().Perm())
		}
	}
	mustExist(t, filepath.Join(p.ConfigDir, "operator.yaml"))
	mustExist(t, filepath.Join(p.ConfigDir, "nova.conf"))
	if !uc.called {
		t.Fatal("operator user not created")
	}
	mustExist(t, p.Sentinel)
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
}
