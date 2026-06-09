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

const testSetupToken = "test-bootstrap-token-abc123"

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

// TestSetup_MissingTokenRejects verifies that an API request without the
// bootstrap token header returns 401 with code "setup_token_required".
func TestSetup_MissingTokenRejects(t *testing.T) {
	h := newTestSetup(t)
	// Send a request to an API route WITHOUT the token header.
	req := httptest.NewRequest(http.MethodGet, "/setup/state", strings.NewReader(""))
	rr := httptest.NewRecorder()
	h.Serve(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "setup_token_required") {
		t.Fatalf("expected setup_token_required in body, got %s", rr.Body.String())
	}
}

// TestSetup_WrongTokenRejects verifies that a wrong token value also yields 401.
func TestSetup_WrongTokenRejects(t *testing.T) {
	h := newTestSetup(t)
	req := httptest.NewRequest(http.MethodGet, "/setup/state", strings.NewReader(""))
	req.Header.Set("X-Nova-Setup-Token", "definitely-wrong-token")
	rr := httptest.NewRecorder()
	h.Serve(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestSetup_StaticNoTokenRequired verifies that the static-serving branch does
// NOT require the bootstrap token (the operator must load the page to enter it).
// When DistDir is empty the static branch returns 404 — but specifically NOT 401.
func TestSetup_StaticNoTokenRequired(t *testing.T) {
	h := newTestSetup(t)
	// Hit a path that falls through to the static branch (not an API route).
	req := httptest.NewRequest(http.MethodGet, "/setup/", strings.NewReader(""))
	// No token header set.
	rr := httptest.NewRecorder()
	h.Serve(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("static branch must not return 401 (got %d): token guard must not gate static serving", rr.Code)
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
		Token: testSetupToken,
	})
	if h == nil {
		t.Fatal("handler must be non-nil when sentinel absent")
	}
	return h
}

func doSetup(t *testing.T, h *SetupHandler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("X-Nova-Setup-Token", testSetupToken)
	rr := httptest.NewRecorder()
	h.Serve(rr, req)
	return rr
}
