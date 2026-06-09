package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/setup"
	"github.com/stretchr/testify/require"
)

func TestWriteCredentialsIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	require.NoError(t, writeCredentials(path, credentials{BaseURL: "https://nova/", AccessToken: "a", RefreshToken: "r", KID: "k"}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestCredsPathHonorsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := credsPath()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "nova", "credentials.json"), p)
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	cmdErr := fn()
	require.NoError(t, w.Close())
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), cmdErr
}

func TestSignedURLSignPrintsURL(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url": "/blob/bafyX?aud=https%3A%2F%2Fe.example&exp=2000000000&kid=k1&sig=abc",
			"kid": "k1", "exp": 2000000000,
		})
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	credPath, err := credsPath()
	require.NoError(t, err)
	require.NoError(t, writeCredentials(credPath, credentials{BaseURL: srv.URL, AccessToken: "tok"}))

	out, err := captureStdout(t, func() error {
		return cmdSignedURLSign([]string{"--path", "/blob/bafyX", "--ttl", "300", "--aud", "https://e.example"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/signed-urls/sign", gotPath)
	require.Equal(t, "Bearer tok", gotAuth)
	require.Contains(t, gotBody, `"path":"/blob/bafyX"`)
	require.Contains(t, gotBody, `"ttl_seconds":300`)
	require.Contains(t, strings.TrimSpace(out), srv.URL+"/blob/bafyX?")
}

func TestSignedURLSignRequiresFlags(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdSignedURLSign([]string{"--path", "/blob/bafyX"}) // missing --aud
	require.Error(t, err)
	require.Contains(t, err.Error(), "required")
}

func TestSignedURLSignNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdSignedURLSign([]string{"--path", "/blob/bafyX", "--aud", "https://e.example"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}

// writeCreds writes credentials.json for the given httptest server URL into a
// fresh XDG_CONFIG_HOME directory and returns the directory so the test can
// set the env var.
func writeCreds(t *testing.T, srvURL, token string) {
	t.Helper()
	credPath, err := credsPath()
	require.NoError(t, err)
	require.NoError(t, writeCredentials(credPath, credentials{BaseURL: srvURL, AccessToken: token}))
}

func TestModerationQuarantinePOSTs(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"quarantined"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "testbearer")

	out, err := captureStdout(t, func() error {
		return cmdModerationQuarantine([]string{"bafyX", "--reason", "r", "--tombstone-after", "14d"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/moderation/quarantine", gotPath)
	require.Equal(t, "Bearer testbearer", gotAuth)
	require.Equal(t, "bafyX", gotBody["cid"])
	require.Equal(t, "r", gotBody["reason"])
	require.Equal(t, "14d", gotBody["tombstone_after"])
	require.Contains(t, out, "quarantined")
}

func TestModerationQuarantineLegalHoldSetsRule(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"quarantined"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "tok")

	_, err := captureStdout(t, func() error {
		return cmdModerationQuarantine([]string{"bafyY", "--legal-hold"})
	})
	require.NoError(t, err)
	require.Equal(t, "severe_content", gotBody["rule"])
	require.Equal(t, true, gotBody["legal_hold"])
}

func TestModerationQuarantineMissingCID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdModerationQuarantine([]string{"--reason", "r"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cid")
}

func TestModerationQuarantineNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdModerationQuarantine([]string{"bafyX"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not logged in")
}

func TestModerationTakedownNoConfirmPOSTs(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"taken_down"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "tok2")

	out, err := captureStdout(t, func() error {
		return cmdModerationTakedown([]string{"bafyZ", "--reason", "dmca", "--no-confirm"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/moderation/takedown", gotPath)
	require.Equal(t, "Bearer tok2", gotAuth)
	require.Equal(t, "bafyZ", gotBody["cid"])
	require.Equal(t, "dmca", gotBody["reason"])
	require.Contains(t, out, "taken_down")
}

func TestModerationTakedownMissingCID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := cmdModerationTakedown([]string{"--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cid")
}

func TestModerationClearLegalHoldNoConfirmPOSTs(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"hold_cleared"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "tok3")

	out, err := captureStdout(t, func() error {
		return cmdModerationClearLegalHold([]string{"bafyW", "--case-id", "case-42", "--reason", "cleared", "--no-confirm"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/moderation/clear-legal-hold", gotPath)
	require.Equal(t, "bafyW", gotBody["cid"])
	require.Equal(t, "case-42", gotBody["case_ref"])
	require.Contains(t, out, "hold_cleared")
}

func TestModerationRestorePOSTs(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"restored"}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "tok4")

	out, err := captureStdout(t, func() error {
		return cmdModerationRestore([]string{"bafyV", "--reason", "false positive"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/moderation/restore", gotPath)
	require.Equal(t, "bafyV", gotBody["cid"])
	require.Equal(t, "false positive", gotBody["reason"])
	require.Contains(t, out, "restored")
}

func TestModerationListGETs(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[],"pagination":{"total":0}}`))
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "tok5")

	out, err := captureStdout(t, func() error {
		return cmdModerationList([]string{"--per-page", "5"})
	})
	require.NoError(t, err)
	require.Equal(t, "/api/v1/admin/moderation/queue", gotPath)
	require.Equal(t, "per_page=5", gotQuery)
	require.Equal(t, "Bearer tok5", gotAuth)
	// Output should be pretty-printed JSON.
	require.Contains(t, out, "pagination")
	require.Contains(t, out, "\n")
}

func TestModerationQuarantine401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "expired")

	err := cmdModerationQuarantine([]string{"bafyX"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized")
}

func TestModerationQuarantine403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "no-role")

	err := cmdModerationQuarantine([]string{"bafyX"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "forbidden")
}

func TestModerationClearLegalHold403OperatorMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCreds(t, srv.URL, "moderator-token")

	err := cmdModerationClearLegalHold([]string{"bafyX", "--no-confirm"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "operator")
}

func TestKeysDispatch(t *testing.T) {
	if err := cmdKeys(nil); err == nil {
		t.Fatal("keys with no subcommand must error")
	}
	if err := cmdKeys([]string{"rotate-master", "--from", "v1"}); err == nil {
		t.Fatal("rotate-master without --to must error")
	}
	if err := cmdKeys([]string{"bogus"}); err == nil {
		t.Fatal("unknown subcommand must error")
	}
}

func TestLoadAnswersFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte("hostname: img.example.com\ncontact_email: a@b.co\nadmin_email: op@b.co\nadmin_password: correcthorsebattery\ntls_mode: dev-self-signed\nauth_mode: local\n"), 0o644)
	if _, err := loadAnswersFile(good); err != nil {
		t.Fatalf("valid answers file rejected: %v", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("hostname: \"\"\ncontact_email: a@b.co\n"), 0o644)
	if _, err := loadAnswersFile(bad); err == nil {
		t.Fatal("missing-hostname answers file must error")
	}
}

type fakeUC struct{ called bool }

func (f *fakeUC) CreateOperator(_ context.Context, _, _ string) error { f.called = true; return nil }

func TestCommitSetup(t *testing.T) {
	root := t.TempDir()
	p := setup.Paths{ConfigDir: filepath.Join(root, "c"), SecretsDir: filepath.Join(root, "s"), Sentinel: filepath.Join(root, "c", ".bootstrap-complete")}
	a := setup.Answers{Hostname: "img.example.com", ContactEmail: "a@b.co", AdminEmail: "op@b.co", AdminPassword: "correcthorsebattery", TLSMode: "dev-self-signed", AuthMode: "local"}
	uc := &fakeUC{}
	var out bytes.Buffer
	if err := commitSetup(context.Background(), a, p, uc, &out); err != nil {
		t.Fatalf("commitSetup: %v", err)
	}
	if !uc.called {
		t.Fatal("operator not created")
	}
	if !strings.Contains(out.String(), "MASTER KEY") {
		t.Fatal("master key not displayed for backup")
	}
	if _, err := os.Stat(p.Sentinel); err != nil {
		t.Fatalf("sentinel not written: %v", err)
	}
}
