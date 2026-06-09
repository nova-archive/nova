package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/setup"
)

func TestNewSetup_NilWhenSentinelPresent(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, ".bootstrap-complete")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewSetup(SetupConfig{Paths: setup.Paths{Sentinel: sentinel}})
	if h != nil {
		t.Fatal("setup handler must be nil when the sentinel is present")
	}
}

func TestSetup_State(t *testing.T) {
	h := newTestSetup(t)
	rr := doSetup(t, h, http.MethodGet, "/setup/state", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("state: got %d", rr.Code)
	}
}

func TestSetup_GenerateMasterKeyReturnsFingerprint(t *testing.T) {
	h := newTestSetup(t)
	rr := doSetup(t, h, http.MethodPost, "/setup/keys/master", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("keys/master: got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MasterKeyHex string `json:"master_key_hex"`
		Fingerprint  string `json:"fingerprint"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want, _ := setup.Fingerprint(resp.MasterKeyHex)
	if resp.Fingerprint == "" || resp.Fingerprint != want {
		t.Fatalf("fingerprint mismatch: resp=%q want=%q", resp.Fingerprint, want)
	}
}

func TestSetup_AnswersValidationRejects(t *testing.T) {
	h := newTestSetup(t)
	rr := doSetup(t, h, http.MethodPost, "/setup/answers", `{"hostname":""}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid answers: got %d, want 400", rr.Code)
	}
}

// helpers
func newTestSetup(t *testing.T) *SetupHandler {
	t.Helper()
	root := t.TempDir()
	h := NewSetup(SetupConfig{
		Paths: setup.Paths{
			ConfigDir:  filepath.Join(root, "config"),
			SecretsDir: filepath.Join(root, "secrets"),
			Sentinel:   filepath.Join(root, "config", ".bootstrap-complete"),
		},
	})
	if h == nil {
		t.Fatal("handler must be non-nil when sentinel absent")
	}
	return h
}

func doSetup(t *testing.T, h *SetupHandler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.Serve(rr, req)
	return rr
}
