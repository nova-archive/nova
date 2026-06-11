# M13 Setup Wizard + Docker Production Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Nova's first-run setup wizard (web SPA + headless `novactl setup`) and the Docker production topology — `internal/setup` domain core, a `.bootstrap-complete` sentinel reduced-boot mode, two-vhost templated nginx, TLS modes, a multi-stage Debian/glibc image, and `operator.yaml` wired into the coordinator — so a fresh checkout becomes a live node via `docker compose --profile setup up`.

**Architecture:** A UI-agnostic `internal/setup` core (answers + keygen + render + tls + commit) is shared verbatim by a nil-gated, sentinel-gated `/setup/*` coordinator seam (driving the `web/setup` React wizard) and by `novactl setup`. The coordinator folds a reduced "setup mode" (DB + `/setup/*` only, no keystore/Kubo) into its boot path, gated on the sentinel; the wizard's atomic commit stages secrets → writes `operator.yaml` + the two-vhost `nova.conf` → creates the operator → writes the sentinel **last** → re-execs into normal mode. `config.LoadFromFile` becomes the coordinator's canonical config source with env overrides preserved. The public/admin split is enforced entirely in the wizard-rendered nginx config.

**Tech Stack:** Go (chi, pgx/sqlc-gen, crypto/rand, crypto/x509, text/template, embed), React + Vite (hermetic, Node-16-safe pins mirroring `web/admin`), nginx, Docker multi-stage (Debian-slim), certbot.

**Spec:** `docs/superpowers/specs/phase1/2026-06-08-phase1-m13-setup-wizard-design.md`

**Conventions (from prior milestones):**
- TDD: failing test → run-fail → minimal impl → run-pass → commit.
- gofmt only the Go files you touch (toolchain-skew rule); `golangci-lint`/`eslint` are CI-side.
- Conventional commits, `(m13)` suffix (e.g. `feat(setup): ... (m13)`). End commit bodies with the `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer.
- Integration tests are `-short`-skippable (testcontainers Postgres).
- Work on branch `m13-setup-wizard` (already created; the design doc is already committed there).

---

## File structure

**Created:**
- `internal/setup/answers.go` — `Answers` model + `Validate` (per-step + final).
- `internal/setup/keygen.go` — CSPRNG master key / swarm key / Ed25519 seed + `Fingerprint`.
- `internal/setup/render.go` — `RenderOperatorYAML` (self-validated) + `RenderNginx` (two-vhost, embedded template).
- `internal/setup/templates/nova.conf.tmpl` — the two-vhost nginx template (Go `embed`).
- `internal/setup/tls.go` — per-mode: `dev-self-signed` CA/leaf gen, `static` validate, `http-01` webroot, `dns-01`/`onion` handoff text.
- `internal/setup/commit.go` — `Commit` atomic finalize (secrets → config → user → sentinel-last).
- `internal/setup/{answers,keygen,render,tls,commit}_test.go` — unit tests.
- `internal/api/handlers/setup.go` — `/setup/*` JSON API + `web/setup` static; nil + sentinel gated.
- `internal/api/handlers/setup_test.go`.
- `internal/integration/m13_setup_wizard_test.go` — nginx-fronted two-vhost split + sentinel flip.
- `web/setup/{package.json,vite.config.ts,tsconfig.json,index.html}` + `src/{main.tsx,Wizard.tsx,api/client.ts,steps/*,*.test.ts}` + `src/wizard.css`.
- `docker/Dockerfile` — multi-stage go-builder → node-builder → Debian-slim runtime (non-root).
- `docker/init/entrypoint.sh` — migrate → sentinel check → exec (setup vs normal).
- `docker/nginx/bootstrap.conf` — `/setup/*`-only first-run config.

**Modified:**
- `cmd/coordinator/main.go` — load `operator.yaml` (canonical) + env overrides; sentinel-gated setup mode.
- `cmd/setup-wizard/main.go` — thin alias → coordinator setup-mode entry (replaces `.gitkeep`).
- `cmd/novactl/main.go` — add `setup` subcommand.
- `internal/api/server.go` — `ServerConfig.Setup *handlers.SetupHandler`; mount `/setup*` (nil-gated).
- `pkg/coordinator/coordinator.go` — `SetupConfig{DistDir, SentinelPath}`; setup-mode boot helper; build setup handler.
- `docker/docker-compose.yml` — coordinator + nginx services; `setup`/`prod` profiles; volumes.
- `docker/.env.example`.
- `nginx/nova.conf.example` — single-origin → two-vhost reference.
- `Makefile` — `setup-{install,build,lint,test}`, `docker-build`, `web` aggregate.
- `.github/workflows/ci.yml` — `web-setup` lane + `docker-build` lane.
- `package.json` / `package-lock.json` — add `web/setup` to workspaces.
- Docs: `docs/ROADMAP.md`, `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`, `docs/THREAT_MODEL.md`, `docs/specs/openapi.yaml`, `docs/legal/OPERATOR_CHECKLIST.md`, `README.md`.

**Reused unchanged:** `internal/config` (loader + floors), `internal/auth/password` + `internal/db/gen` (`CreateUser`/`UserRoleOperator`), `internal/api/handlers/admin_spa.go` (seam pattern), `scripts/hermetic-spa.sh`, the `internal/integration` harness, `web/admin` build conventions.

---

## Task 1: `internal/setup` — answers model + keygen

**Files:**
- Create: `internal/setup/answers.go`, `internal/setup/keygen.go`
- Test: `internal/setup/answers_test.go`, `internal/setup/keygen_test.go`

- [ ] **Step 1: Write failing tests for keygen.**

```go
// internal/setup/keygen_test.go
package setup

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateMasterKey(t *testing.T) {
	k1, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	raw, err := hex.DecodeString(k1)
	if err != nil || len(raw) != 32 {
		t.Fatalf("master key must be 64 hex chars / 32 bytes, got %d bytes (err=%v)", len(raw), err)
	}
	k2, _ := GenerateMasterKey()
	if k1 == k2 {
		t.Fatal("two generations must differ (CSPRNG)")
	}
}

func TestFingerprint(t *testing.T) {
	// fingerprint is the first 8 bytes of the master key, lowercase hex (16 chars).
	fp, err := Fingerprint("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp != "0011223344556677" {
		t.Fatalf("fingerprint = %q, want 0011223344556677", fp)
	}
	if _, err := Fingerprint("xyz"); err == nil {
		t.Fatal("Fingerprint must reject non-hex")
	}
}

func TestGenerateSwarmKey(t *testing.T) {
	sk, err := GenerateSwarmKey()
	if err != nil {
		t.Fatalf("GenerateSwarmKey: %v", err)
	}
	if !strings.HasPrefix(sk, "/key/swarm/psk/1.0.0/\n/base16/\n") {
		t.Fatalf("swarm key missing Kubo PSK header:\n%s", sk)
	}
	body := strings.TrimSpace(sk[strings.LastIndex(sk, "\n")+1:])
	if raw, err := hex.DecodeString(body); err != nil || len(raw) != 32 {
		t.Fatalf("swarm key body must be 32 bytes hex, got %d (err=%v)", len(raw), err)
	}
}

func TestGenerateSigningSeed(t *testing.T) {
	seed, err := GenerateSigningSeed()
	if err != nil {
		t.Fatalf("GenerateSigningSeed: %v", err)
	}
	if raw, err := hex.DecodeString(seed); err != nil || len(raw) != 32 {
		t.Fatalf("ed25519 seed must be 32 bytes hex, got %d (err=%v)", len(raw), err)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/setup/ -run 'Key|Fingerprint|Seed' -v` → undefined functions).

- [ ] **Step 3: Implement `keygen.go`.**

```go
// Package setup is the UI-agnostic first-run setup domain core, shared by the
// /setup/* coordinator seam (web wizard) and by `novactl setup`. It generates
// key material, renders operator.yaml + the two-vhost nova.conf, and performs
// the atomic first-run commit.
package setup

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateMasterKey returns a fresh 32-byte master key as 64 lowercase hex chars.
func GenerateMasterKey() (string, error) { return randHex(32) }

// GenerateSigningSeed returns a fresh 32-byte Ed25519 seed as hex. The seed
// expands to a full ed25519 private key via ed25519.NewKeyFromSeed; we validate
// that here so a bad CSPRNG read fails loudly.
func GenerateSigningSeed() (string, error) {
	h, err := randHex(ed25519.SeedSize)
	if err != nil {
		return "", err
	}
	raw, _ := hex.DecodeString(h)
	_ = ed25519.NewKeyFromSeed(raw) // panics on wrong length; SeedSize guarantees correctness
	return h, nil
}

// GenerateSwarmKey returns a fresh IPFS private-network swarm key in Kubo's
// PSK v1 base16 wire format.
func GenerateSwarmKey() (string, error) {
	body, err := randHex(32)
	if err != nil {
		return "", err
	}
	return "/key/swarm/psk/1.0.0/\n/base16/\n" + body + "\n", nil
}

// Fingerprint returns the first 8 bytes of a hex master key as 16 lowercase hex
// chars — the forced-readback challenge shown during setup.
func Fingerprint(masterKeyHex string) (string, error) {
	raw, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return "", fmt.Errorf("setup: fingerprint: %w", err)
	}
	if len(raw) < 8 {
		return "", fmt.Errorf("setup: fingerprint: key too short (%d bytes)", len(raw))
	}
	return hex.EncodeToString(raw[:8]), nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("setup: csprng: %w", err)
	}
	return hex.EncodeToString(b), nil
}
```

- [ ] **Step 4: Write failing tests for the `Answers` model.**

```go
// internal/setup/answers_test.go
package setup

import "testing"

func validAnswers() Answers {
	return Answers{
		Hostname:      "img.example.com",
		ContactEmail:  "abuse@example.com",
		AdminEmail:    "op@example.com",
		AdminPassword: "correct horse battery", // >= 12 chars
		TLSMode:       "dev-self-signed",
		AuthMode:      "local",
	}
}

func TestAnswersValidate_OK(t *testing.T) {
	if err := validAnswers().Validate(); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}
}

func TestAnswersValidate_Rejections(t *testing.T) {
	cases := map[string]func(*Answers){
		"missing hostname":      func(a *Answers) { a.Hostname = "" },
		"missing contact":       func(a *Answers) { a.ContactEmail = "" },
		"short password":        func(a *Answers) { a.AdminPassword = "short" },
		"bad tls mode":          func(a *Answers) { a.TLSMode = "bogus" },
		"static without paths":  func(a *Answers) { a.TLSMode = "static" },
		"public uploads no tos": func(a *Answers) { a.PublicUploads = true; a.TosURL = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAnswers()
			mut(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s: expected validation error", name)
			}
		})
	}
}
```

- [ ] **Step 5: Implement `answers.go`** (reuse the `config.validate` floors by rendering + loading where overlap exists; do cheap pre-checks here for friendly per-step UX).

```go
package setup

import (
	"fmt"
	"net/mail"
	"strings"
)

// Answers is the operator's first-run choices. Non-secret fields end up in
// operator.yaml; AdminPassword and the generated key material never do.
type Answers struct {
	Hostname     string `json:"hostname" yaml:"hostname"`
	ContactEmail string `json:"contact_email" yaml:"contact_email"`
	DisplayName  string `json:"display_name,omitempty" yaml:"display_name,omitempty"`

	AdminEmail    string `json:"admin_email" yaml:"admin_email"`
	AdminPassword string `json:"admin_password" yaml:"admin_password"` // never persisted to operator.yaml

	TLSMode  string `json:"tls_mode" yaml:"tls_mode"` // dev-self-signed|http-01|dns-01|static|onion
	CertPath string `json:"cert_path,omitempty" yaml:"cert_path,omitempty"`
	KeyPath  string `json:"key_path,omitempty" yaml:"key_path,omitempty"`

	AuthMode      string `json:"auth_mode" yaml:"auth_mode"`             // local|external
	IssuerURL     string `json:"issuer_url,omitempty" yaml:"issuer_url,omitempty"`
	ClientID      string `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	PublicUploads bool   `json:"public_uploads" yaml:"public_uploads"`
	TosURL        string `json:"tos_url,omitempty" yaml:"tos_url,omitempty"`
	Paranoid      bool   `json:"paranoid" yaml:"paranoid"`
}

const minPasswordLen = 12

var validTLSModes = map[string]bool{
	"dev-self-signed": true, "http-01": true, "dns-01": true, "static": true, "onion": true,
}

// Validate runs the friendly pre-checks. The authoritative floors are re-run
// when render.go round-trips the generated operator.yaml through
// config.LoadFromBytes, so this never diverges from the runtime validator.
func (a Answers) Validate() error {
	if strings.TrimSpace(a.Hostname) == "" {
		return fmt.Errorf("setup: hostname is required")
	}
	if _, err := mail.ParseAddress(a.ContactEmail); err != nil {
		return fmt.Errorf("setup: contact_email must be a valid address")
	}
	if _, err := mail.ParseAddress(a.AdminEmail); err != nil {
		return fmt.Errorf("setup: admin_email must be a valid address")
	}
	if len(a.AdminPassword) < minPasswordLen {
		return fmt.Errorf("setup: admin_password must be at least %d characters", minPasswordLen)
	}
	if !validTLSModes[a.TLSMode] {
		return fmt.Errorf("setup: tls_mode must be one of dev-self-signed|http-01|dns-01|static|onion")
	}
	if a.TLSMode == "static" && (a.CertPath == "" || a.KeyPath == "") {
		return fmt.Errorf("setup: tls_mode=static requires cert_path and key_path")
	}
	if a.AuthMode == "external" && (a.IssuerURL == "" || a.ClientID == "") {
		return fmt.Errorf("setup: auth_mode=external requires issuer_url and client_id")
	}
	if a.PublicUploads && a.TosURL == "" {
		return fmt.Errorf("setup: public_uploads requires tos_url (T1.20)")
	}
	return nil
}
```

- [ ] **Step 6: Run all Task-1 tests** (`go test ./internal/setup/ -v`) → PASS. gofmt the two files.

- [ ] **Step 7: Commit.**

```bash
gofmt -w internal/setup/answers.go internal/setup/keygen.go internal/setup/answers_test.go internal/setup/keygen_test.go
git add internal/setup/{answers,keygen}.go internal/setup/{answers,keygen}_test.go
git commit -m "feat(setup): answers model + CSPRNG keygen/fingerprint (m13)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `internal/setup/render.go` — operator.yaml + two-vhost nginx

**Files:**
- Create: `internal/setup/render.go`, `internal/setup/templates/nova.conf.tmpl`
- Test: `internal/setup/render_test.go`

- [ ] **Step 1: Write the failing test.** The operator.yaml render must round-trip through the real loader; the nginx render must emit two server blocks with the boundary-① route map.

```go
// internal/setup/render_test.go
package setup

import (
	"strings"
	"testing"

	"github.com/nova-archive/nova/internal/config"
)

func TestRenderOperatorYAML_RoundTrips(t *testing.T) {
	a := validAnswers()
	a.PublicUploads = true
	a.TosURL = "https://img.example.com/tos"
	out, err := RenderOperatorYAML(a)
	if err != nil {
		t.Fatalf("RenderOperatorYAML: %v", err)
	}
	cfg, err := config.LoadFromBytes(out)
	if err != nil {
		t.Fatalf("rendered operator.yaml does not load: %v\n%s", err, out)
	}
	if cfg.Operator.Hostname != a.Hostname || cfg.TLS.Mode != a.TLSMode || cfg.TosURL != a.TosURL {
		t.Fatalf("round-trip mismatch: %+v", cfg)
	}
	if !cfg.Uploads.PublicUploads {
		t.Fatal("public_uploads lost in round-trip")
	}
}

func TestRenderOperatorYAML_NoSecrets(t *testing.T) {
	a := validAnswers()
	a.AdminPassword = "SUPERSECRETvalue123"
	out, _ := RenderOperatorYAML(a)
	if strings.Contains(string(out), "SUPERSECRET") {
		t.Fatal("admin password must never appear in operator.yaml")
	}
}

func TestRenderNginx_TwoVhostRouteMap(t *testing.T) {
	a := validAnswers()
	conf, err := RenderNginx(a)
	if err != nil {
		t.Fatalf("RenderNginx: %v", err)
	}
	// Two server blocks.
	if strings.Count(conf, "server {") < 3 { // public 443 + admin 8445 + http redirect
		t.Fatalf("expected >=3 server blocks (public/admin/redirect):\n%s", conf)
	}
	// Admin-only routes appear under an admin server_name, public under public.
	mustContain(t, conf, "server_name "+a.Hostname)
	mustContain(t, conf, "location /api/v1/admin")
	mustContain(t, conf, "location /api/v1/auth")
	mustContain(t, conf, "location /widget")
	mustContain(t, conf, "location /fed/") // -> 404 on both
}

func mustContain(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("missing %q in:\n%s", needle, hay)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** (undefined `RenderOperatorYAML`/`RenderNginx`).

- [ ] **Step 3: Create the embedded template** `internal/setup/templates/nova.conf.tmpl`. Base it on `nginx/nova.conf.example` (rate-limit zones, cache, common proxy headers, security headers) but split into **public_host** and **admin_host** server blocks per the spec route map. Use Go `text/template` fields (`{{.Hostname}}`, `{{.AdminListen}}`, `{{.CertPath}}`, `{{.KeyPath}}`, `{{.Upstream}}`, `{{if .ACME}}…{{end}}`). The public block contains `location ~ ^/(blob|i)/`, `/legal/`, `location ~ ^/api/v1/(uploads|blobs|images)`, `/widget`, `/health`, and a default `location / { return 404; }`. The admin block contains `/admin`, `/api/v1/admin/`, `/api/v1/auth/`, `/api/v1/users/me`, `/health`, default `404`. Both: `location /fed/ { return 404; }` and the ACL'd `/metrics`. (Copy the directive bodies from `nova.conf.example` verbatim where they apply.)

- [ ] **Step 4: Implement `render.go`.**

```go
package setup

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	"github.com/nova-archive/nova/internal/config"
	"gopkg.in/yaml.v3"
)

//go:embed templates/nova.conf.tmpl
var templatesFS embed.FS

var nginxTmpl = template.Must(template.ParseFS(templatesFS, "templates/nova.conf.tmpl"))

// RenderOperatorYAML builds operator.yaml from Answers and self-validates by
// round-tripping through the real loader. An un-loadable render is a bug, not
// a runtime surprise.
func RenderOperatorYAML(a Answers) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := config.Config{
		Operator: config.Operator{Hostname: a.Hostname, ContactEmail: a.ContactEmail, DisplayName: a.DisplayName},
		TLS:      config.TLS{Mode: a.TLSMode, CertPath: a.CertPath, KeyPath: a.KeyPath},
		Auth:     config.Auth{IssuerURL: a.IssuerURL, ClientID: a.ClientID, Paranoid: a.Paranoid},
		Moderation: config.Moderation{TakedownDefaultAction: "quarantine"},
		Orchestrator: config.Orchestrator{
			Replication: config.Replication{Factor: config.ReplicationFactor{Important: 3, Normal: 2, Cache: 1}},
		},
		Coordinator: config.Coordinator{PublicIpfsDht: false},
		Uploads:     config.Uploads{PublicUploads: a.PublicUploads},
		TosURL:      a.TosURL,
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("setup: marshal operator.yaml: %w", err)
	}
	if _, err := config.LoadFromBytes(out); err != nil {
		return nil, fmt.Errorf("setup: rendered operator.yaml failed validation: %w", err)
	}
	return out, nil
}

type nginxView struct {
	Hostname    string
	Upstream    string // coordinator addr, e.g. "coordinator:9000"
	CertPath    string
	KeyPath     string
	AdminListen string // e.g. "8445 ssl"
	ACME        bool   // http-01 renders the ACME challenge location
}

// RenderNginx builds the two-vhost nova.conf for the chosen TLS mode.
func RenderNginx(a Answers) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	v := nginxView{
		Hostname:    a.Hostname,
		Upstream:    "coordinator:9000",
		AdminListen: "8445 ssl",
		ACME:        a.TLSMode == "http-01",
	}
	switch a.TLSMode {
	case "static":
		v.CertPath, v.KeyPath = a.CertPath, a.KeyPath
	case "dev-self-signed", "http-01", "dns-01", "onion":
		v.CertPath = "/etc/nova/tls/fullchain.pem"
		v.KeyPath = "/etc/nova/tls/privkey.pem"
	}
	var buf bytes.Buffer
	if err := nginxTmpl.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("setup: render nginx: %w", err)
	}
	return buf.String(), nil
}
```

- [ ] **Step 5: Run** `go test ./internal/setup/ -run Render -v` → PASS. Iterate the `.tmpl` until the route-map assertions pass.

- [ ] **Step 6: Commit** (`feat(setup): operator.yaml + two-vhost nginx rendering (m13)`), gofmt touched Go files.

---

## Task 3: `internal/setup/tls.go` — per-mode TLS

**Files:**
- Create: `internal/setup/tls.go`; Test: `internal/setup/tls_test.go`

- [ ] **Step 1: Failing test** — `dev-self-signed` produces a parseable CA+leaf with the hostname SAN; `static` validates existing PEMs; `dns-01`/`onion` return non-empty handoff text and write no cert.

```go
// internal/setup/tls_test.go
package setup

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionTLS_DevSelfSigned(t *testing.T) {
	dir := t.TempDir()
	a := validAnswers() // dev-self-signed
	res, err := ProvisionTLS(a, dir)
	if err != nil {
		t.Fatalf("ProvisionTLS: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(res.CertPath, res.KeyPath); err != nil {
		t.Fatalf("generated cert/key not loadable: %v", err)
	}
}

func TestProvisionTLS_Handoff(t *testing.T) {
	dir := t.TempDir()
	a := validAnswers()
	a.TLSMode = "dns-01"
	res, err := ProvisionTLS(a, dir)
	if err != nil {
		t.Fatalf("ProvisionTLS dns-01: %v", err)
	}
	if res.HandoffInstructions == "" {
		t.Fatal("dns-01 must return operator-handoff instructions")
	}
	if _, err := os.Stat(filepath.Join(dir, "fullchain.pem")); err == nil {
		t.Fatal("dns-01 must not generate a cert in M13")
	}
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `tls.go`** — `ProvisionTLS(a Answers, tlsDir string) (TLSResult, error)`. For `dev-self-signed`: generate an ECDSA P-256 CA + a leaf (SAN = hostname), write `fullchain.pem`/`privkey.pem` (mode 0600) into `tlsDir`. For `static`: stat + `tls.LoadX509KeyPair(a.CertPath, a.KeyPath)`; return those paths. For `http-01`: return paths under `tlsDir` (certbot fills them in the prod profile) + a note. For `dns-01`/`onion`: write nothing, return `HandoffInstructions` text (provide DNS creds / run Tor + supply self-signed). `TLSResult{CertPath, KeyPath, HandoffInstructions string}`. Use `crypto/ecdsa`, `crypto/x509`, `encoding/pem`.

- [ ] **Step 4: Run** `go test ./internal/setup/ -run TLS -v` → PASS.

- [ ] **Step 5: Commit** (`feat(setup): per-mode TLS provisioning (dev-self-signed/static/handoff) (m13)`).

---

## Task 4: `internal/setup/commit.go` — atomic finalize

**Files:**
- Create: `internal/setup/commit.go`; Test: `internal/setup/commit_test.go`

- [ ] **Step 1: Failing test.** Verify the **sentinel-last** invariant and `0600` secret modes against a temp filesystem; the user-creation step is injected behind an interface so the unit test needs no DB.

```go
// internal/setup/commit_test.go
package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeUserCreator struct{ called bool; role string }

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
	// secrets at 0600
	for _, name := range []string{"master-key-v1", "swarm.key", "oidc-signing-key"} {
		fi, err := os.Stat(filepath.Join(p.SecretsDir, name))
		if err != nil {
			t.Fatalf("missing secret %s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, fi.Mode().Perm())
		}
	}
	// operator.yaml + nova.conf present, user created, sentinel present
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
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `commit.go`.** Define `Paths`, the `UserCreator` interface, and `Commit` with the ordered steps. The master-key label is fixed `v1` for first-run (matches the keystore bootstrap).

```go
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
	CreateOperator(ctx context.Context, email, passwordHash string) error
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

	// 3. operator user (argon2id hash is computed by the caller's UserCreator impl)
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
```

- [ ] **Step 4: Implement the DB-backed `UserCreator`** in `internal/setup/commit_db.go` (kept out of the unit test path):

```go
package setup

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/db/gen"
)

// DBUserCreator creates the operator via the existing CreateUser query + argon2id.
type DBUserCreator struct{ Q *gen.Queries }

func (d DBUserCreator) CreateOperator(ctx context.Context, email, plain string) error {
	hash, err := password.Hash(plain)
	if err != nil {
		return err
	}
	_, err = d.Q.CreateUser(ctx, gen.CreateUserParams{
		Email:        email,
		Role:         gen.UserRoleOperator,
		PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	return err
}
```

- [ ] **Step 5: Run** `go test ./internal/setup/ -v` → PASS. gofmt.

- [ ] **Step 6: Commit** (`feat(setup): atomic first-run commit (secrets->config->user->sentinel-last) (m13)`).

---

## Task 5: `internal/api/handlers/setup.go` — the `/setup/*` seam

**Files:**
- Create: `internal/api/handlers/setup.go`, `internal/api/handlers/setup_test.go`

- [ ] **Step 1: Failing test** (httptest): `NewSetup` returns nil when the sentinel is present; when absent it serves `GET /setup/state` (200 JSON), `POST /setup/keys/master` (returns hex + fingerprint), and rejects an invalid `POST /setup/answers` (400). Mirror `admin_spa_test.go` for the static + CSP assertions.

```go
// internal/api/handlers/setup_test.go (shape)
func TestNewSetup_NilWhenSentinelPresent(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, ".bootstrap-complete")
	os.WriteFile(sentinel, []byte("x"), 0o644)
	if h := NewSetup("", sentinel, nil); h != nil {
		t.Fatal("setup handler must be nil when the sentinel is present")
	}
}

func TestSetup_GenerateMasterKeyReturnsFingerprint(t *testing.T) {
	// POST /setup/keys/master -> {master_key_hex, fingerprint}; fingerprint == first 8 bytes.
}
```

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement `setup.go`.** `NewSetup(distDir, sentinelPath string, uc setup.UserCreator) *SetupHandler` returns nil if `sentinelPath` exists (so the seam is unmounted in normal mode), else builds a handler holding the dist dir + a `setup.Paths` + the `UserCreator`. `Serve(w, r)` routes: `GET /setup/state`, `POST /setup/keys/master` (call `setup.GenerateMasterKey` + `setup.Fingerprint`, stage via the secrets path, return JSON), `POST /setup/answers` (decode → `Answers.Validate` → 400 on error), `POST /setup/commit` (decode answers → `setup.Commit` → 200; on success the coordinator will re-exec), else serve the static bundle from `distDir` with the strict CSP (reuse the admin CSP shape). Use `httputil.WriteError` for errors. Loopback enforcement is a deployment concern (nginx/compose binds `:8444` to `127.0.0.1`); the handler itself need not check the peer, but document that.

- [ ] **Step 4: Run** `go test ./internal/api/handlers/ -run Setup -v` → PASS.

- [ ] **Step 5: Commit** (`feat(api): /setup/* seam — state/keys/answers/commit, sentinel-gated, CSP (m13)`).

---

## Task 6: Config reconciliation — wire `operator.yaml` into `cmd/coordinator`

**Files:**
- Modify: `cmd/coordinator/main.go`
- Test: `cmd/coordinator/main_test.go` (new, table-driven over a small `loadBaseConfig` helper)

- [ ] **Step 1: Failing test.** Extract the precedence into a testable helper `resolveOperatorConfig(path string, env func(string) string) (baseValues, error)` that: loads `operator.yaml` when `path` is non-empty/exists, then applies env overrides. Assert: (a) file value used when env unset; (b) env wins when set; (c) missing file + env-only still works (back-compat).

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** Add a `loadOperatorConfig()` step at the top of `run()`: read `NOVA_CONFIG_FILE` (default `$NOVA_CONFIG_DIR/operator.yaml`); if present, `config.LoadFromFile`; seed locals (`publicUploads`, `tosURL`, `paranoid`/`recordIP`, hostname→`NOVA_AUTH_ISSUER` default, `tls`/auth mode where the coordinator consumes them) from the `*config.Config`; then let the existing `os.Getenv` reads override (an env var set ⇒ wins). Keep all current env defaults intact. Do **not** touch the M7–M12 tuning knob reads. Per the spec precedence table.

- [ ] **Step 4: Run** `go test ./cmd/coordinator/ -v` → PASS; `go build ./...` green; the existing integration tests (env-driven) still compile.

- [ ] **Step 5: Commit** (`feat(coordinator): operator.yaml canonical config + env overrides (m13)`).

---

## Task 7: Coordinator setup-mode reduced boot + seam wiring

**Files:**
- Modify: `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`, `cmd/setup-wizard/main.go`

- [ ] **Step 1: Failing test** (`pkg/coordinator` or `internal/api`): with `ServerConfig.Setup` non-nil, `GET /setup/state` routes to it; with it nil, `/setup/state` → 404. Add to `server_test.go` (mirror the admin/widget nil-gating tests).

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.**
  - `internal/api/server.go`: add `Setup *handlers.SetupHandler` to `ServerConfig`; mount `r.Handle("/setup", ...)` + `r.Handle("/setup/*", ...)` when non-nil (mirror `/admin*`, `:107`–`:110`).
  - `pkg/coordinator/coordinator.go`: add `SetupConfig{DistDir, SentinelPath string}` to `Config`; build `handlers.NewSetup(...)` and assign `sc.Setup` (sibling to `:399`–`:400`). Add a `RunSetupMode(ctx)` entry (or a `SetupOnly bool` on `Config`) that builds a **reduced** server — DB pool + the setup handler only, **no** keystore/Kubo/auth/upload/audit boot — and serves it.
  - `cmd/coordinator/main.go`: at the top of `run()`, after `loadOperatorConfig`, check the sentinel (`$NOVA_CONFIG_DIR/.bootstrap-complete`). **Absent** ⇒ open the DB pool, run migrations-assumed-done, build `coordinator` in setup-only mode (`SetupConfig.DistDir = $NOVA_SETUP_DIST_DIR`), serve `/setup/*`, return. **Present** ⇒ the existing full boot, with `SetupConfig.SentinelPath` set so `NewSetup` returns nil (`/setup` 404).
  - `cmd/setup-wizard/main.go`: a thin `main()` that sets a marker and calls the coordinator's setup-mode entry (or simply documents that `coordinator` auto-detects setup mode; keep the binary as a discoverable alias).

- [ ] **Step 4: Run** `go test ./internal/api/ ./pkg/coordinator/ -v` and `go build ./...` → PASS.

- [ ] **Step 5: Commit** (`feat(coordinator): sentinel-gated setup mode + /setup seam wiring (m13)`).

---

## Task 8: `novactl setup` subcommand

**Files:**
- Modify: `cmd/novactl/main.go`; Test: `cmd/novactl/main_test.go`

- [ ] **Step 1: Failing test.** `novactl setup --config-file <answers.yaml>` parses YAML into `setup.Answers`, validates, and (with a stub `UserCreator`/temp `Paths`) commits; a missing required field exits non-zero with a clear message. (`--interactive` is exercised manually.)

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement.** Add `case "setup":` to the top-level dispatch (alongside `auth|signed-url|moderation|keys`). Flags: `--interactive`, `--config-file`. For `--config-file`: `yaml.Unmarshal` into `setup.Answers`, `Validate`, resolve `Paths` from `NOVA_CONFIG_DIR`/`NOVA_SECRETS_DIR` env (or flags), open the DB (`DATABASE_URL`), `setup.Commit(ctx, ans, paths, setup.DBUserCreator{Q: gen.New(pool)})`. For `--interactive`: prompt each field on stdin including the typed fingerprint readback (re-prompt until it matches `setup.Fingerprint`). Update the usage string.

- [ ] **Step 4: Run** `go test ./cmd/novactl/ -v` → PASS; `go build ./...`.

- [ ] **Step 5: Commit** (`feat(novactl): setup subcommand (--interactive | --config-file) (m13)`).

---

## Task 9: `web/setup` scaffold + workspace + build/CI

**Files:**
- Create: `web/setup/{package.json,vite.config.ts,tsconfig.json,index.html,src/main.tsx,src/api/client.ts}`
- Modify: `package.json`, `Makefile`, `.github/workflows/ci.yml`

- [ ] **Step 1:** Scaffold `web/setup` mirroring `web/admin` exactly (Node-16-safe pins: Vite `^4.5`, Vitest `^0.34`, TypeScript `^5.3`, React 18, jsdom `^22.1`); `vite.config.ts` `base: '/setup/'`, hashed assets; `tsconfig.json` copied from `web/admin`. `package.json` name `@nova/setup`; scripts `build`/`lint`(`tsc --noEmit`)/`test`(`vitest run`). `src/api/client.ts` mirrors `web/admin/src/api/client.ts` (same-origin fetch wrapper) targeting `/setup/*`.

- [ ] **Step 2:** Add `web/setup` to root `package.json` `workspaces`; run `npm install` to regenerate `package-lock.json`; verify `npm ci` on Node 20 (CI) and that `npm install` works on the local Node 16 host.

- [ ] **Step 3:** Makefile: add `setup-install`, `setup-build` (`npm run -w @nova/setup build`), `setup-lint`, `setup-test`, and fold into a `web` aggregate; add `docker-build` (`docker build -f docker/Dockerfile -t nova-coordinator:dev .`). Mirror the `admin-*`/`widget-*` target shapes.

- [ ] **Step 4:** `.github/workflows/ci.yml`: add a `web-setup` job (or extend the existing web job): `setup-node@v4` Node 20, `npm ci`, `make setup-lint setup-test setup-build`, then `scripts/hermetic-spa.sh web/setup/dist`. Add a `docker-build` job that runs `make docker-build` (no push).

- [ ] **Step 5:** Run `make setup-build && scripts/hermetic-spa.sh web/setup/dist` → PASS (empty app builds clean + hermetic).

- [ ] **Step 6: Commit** (`build(setup): web/setup workspace + Makefile/CI + hermetic gate (m13)`).

---

## Task 10: `web/setup` Wizard — stepper, readback gate, orientation

**Files:**
- Create: `web/setup/src/Wizard.tsx`, `web/setup/src/steps/*.tsx`, `web/setup/src/wizard.css`, `web/setup/src/*.test.ts`

- [ ] **Step 1: Failing Vitest tests** (jsdom): the stepper blocks advancing past the master-key step until the typed fingerprint equals the generated one; the backup download blob contains exactly `master_key_hex` + `fingerprint`; the orientation page renders the `<script>` embed snippet + the admin URL after a mocked `200` commit. (Mock `fetch` to the `/setup/*` API.)

- [ ] **Step 2: Run, expect FAIL.**

- [ ] **Step 3: Implement** the linear stepper (`Wizard.tsx` + one component per step): Welcome → MasterKey (display hex + fingerprint, "download backup .txt", typed-readback gate) → Keys (swarm/signing auto, just a confirmation) → AdminUser → TLSMode (per-mode privacy-cost copy) → ToS → Paranoid → Review → Commit → Orientation. Drive everything through `api/client.ts`. Brand-tokened CSS mirroring `web/admin`.

- [ ] **Step 4: Run** `make setup-test` → PASS; `make setup-build && scripts/hermetic-spa.sh web/setup/dist` → PASS.

- [ ] **Step 5: Commit** (`feat(setup): wizard stepper + master-key readback gate + orientation page (m13)`).

---

## Task 11: Docker — image, entrypoint, compose profiles

**Files:**
- Create: `docker/Dockerfile`, `docker/init/entrypoint.sh`, `docker/nginx/bootstrap.conf`
- Modify: `docker/docker-compose.yml`, `docker/.env.example`

- [ ] **Step 1: `docker/Dockerfile`** — multi-stage:
  1. `go-builder` (golang:1.26-bookworm): build `coordinator`, `novactl`, the migrate tool with cgo (libvips dev libs installed) — `CGO_ENABLED=1`, `-trimpath -ldflags="-s -w"`.
  2. `node-builder` (node:20-bookworm): `npm ci`; `make admin-build widget-build setup-build` → the three `dist/` trees.
  3. runtime (`debian:bookworm-slim`): install runtime libvips + ca-certificates; create non-root `nova` UID; copy the three binaries, the entrypoint, and the static bundles to known paths; `USER nova`; `ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]`.

- [ ] **Step 2: `docker/init/entrypoint.sh`** (sh, `set -eu`):

```sh
#!/bin/sh
set -eu
# Run forward-only migrations (idempotent).
/usr/local/bin/migrate -database "$DATABASE_URL" up
# Sentinel decides setup vs normal mode. The coordinator auto-detects the same
# file; we log the chosen mode for operator clarity.
if [ -f "${NOVA_CONFIG_DIR:-/etc/nova}/.bootstrap-complete" ]; then
  echo "entrypoint: .bootstrap-complete present -> normal mode"
else
  echo "entrypoint: .bootstrap-complete absent -> SETUP mode (/setup only, loopback :8444)"
fi
exec /usr/local/bin/coordinator
```

- [ ] **Step 3: `docker/nginx/bootstrap.conf`** — a single server block listening on `:8444` that proxies only `location /setup/ { proxy_pass http://coordinator:9000; }` and `location = /health`; everything else `404`.

- [ ] **Step 4: `docker/docker-compose.yml`** — keep postgres; add:
  - `coordinator` (build `../`, `-f docker/Dockerfile`): env from `.env` (`DATABASE_URL`, `NOVA_KUBO_REPO`, `NOVA_CONFIG_DIR=/etc/nova`, `NOVA_SECRETS_DIR=/run/secrets`, dist-dir envs, `NOVA_MASTER_KEY_ACTIVE=v1`, secret file envs pointing at `/run/secrets/*`); volumes `nova-config:/etc/nova`, `nova-secrets:/run/secrets`, `nova-kubo:/var/lib/kubo`, `nova-tmp:/var/tmp/nova-uploads`; non-root; `depends_on: postgres (healthy)`.
  - `nginx` (`nginx:1.25-alpine`): mounts `nova-config` (for the rendered `nova.conf` + `tls/`); ports `8442:80`, `8443:443`, `8445:8445` (admin, bound `127.0.0.1`). Under the **`setup` profile**, mount `bootstrap.conf` and publish `127.0.0.1:8444:8444`. Under **`prod`**, add a `certbot` service (ACME webroot, renew loop).
  - profiles: `setup`, `prod`. Document `docker compose --profile setup up` for first-run.
  - volumes block: `nova-config`, `nova-secrets`, `nova-kubo`, `nova-tmp` (+ existing `postgres-data`).

- [ ] **Step 5: `.env.example`** — `POSTGRES_PASSWORD`, the `NOVA_*` runtime knobs the compose passes, and a comment that secrets are generated by the wizard into `nova-secrets`.

- [ ] **Step 6:** `docker build -f docker/Dockerfile -t nova-coordinator:dev .` → image builds. `docker compose -f docker/docker-compose.yml --profile setup config` → valid. (Full `up` is the human-action checklist.)

- [ ] **Step 7: Commit** (`build(docker): multi-stage image + entrypoint sentinel + setup/prod compose profiles (m13)`).

---

## Task 12: nginx-fronted integration test — two-vhost split + sentinel flip

**Files:**
- Create: `internal/integration/m13_setup_wizard_test.go`

- [ ] **Step 1: Failing test.** Reuse `startCoordinatorWithNginxCfg` / `seedAuthUser` / the testcontainers Postgres.
  - **Two-vhost split (normal mode):** render a two-vhost `nova.conf` via `setup.RenderNginx`; boot the coordinator (normal mode, sentinel present) behind it; assert: `GET /api/v1/admin/...` on the **public_host** `server_name`/Host → `404`; `GET /api/v1/auth/config` on public_host → `404`; `GET /blob/{cid}` on the **admin_host** → `404`; `GET /fed/v1/...` → `404` on both. (Drive the Host header to select the vhost.)
  - **Sentinel flip:** boot with the sentinel **absent** → `GET /setup/state` `200`, a steady-state route `404`; (optionally run a commit against the test DB) boot with the sentinel **present** → `GET /setup/state` `404`, steady-state routes served.

- [ ] **Step 2: Run, expect FAIL** (`go test ./internal/integration/ -run M13 -v`).

- [ ] **Step 3: Implement** the test + any small harness helper (`startNginxM13` mirroring `startNginxM11`). gofmt.

- [ ] **Step 4: Run** `go test ./internal/integration/ -run M13 -v` → PASS; `go test -short ./...` green.

- [ ] **Step 5: Commit** (`test(m13): nginx-fronted two-vhost split + setup→normal sentinel flip (m13)`).

---

## Task 13: Documentation reconciliations + nginx reference

**Files:**
- Modify: `docs/ROADMAP.md`, `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`, `docs/THREAT_MODEL.md`, `docs/specs/openapi.yaml`, `docs/legal/OPERATOR_CHECKLIST.md`, `README.md`, `nginx/nova.conf.example`

- [ ] **Step 1: `docs/ROADMAP.md`** — flesh out the M13 row (status pending→done on completion; link this design + plan; tag `m13-setup-wizard`; record deferrals: hardening/signing/CI-smoke/quickstart → M14, dns-01/onion automation → later, operator.yaml tuning-knob decode → later).
- [ ] **Step 2: Master plan** — M13 status/links; reconcile "Onboarding wizard" (setup mode folded into the coordinator; operator.yaml canonical+env-override) + container-topology with what shipped.
- [ ] **Step 3: `docs/THREAT_MODEL.md`** — boundaries ① / ①a now *implemented*; note widget on public_host.
- [ ] **Step 4: `docs/specs/openapi.yaml`** — note-only block describing the ephemeral, sentinel-gated `/setup/*` surface (mirror the M11/M12 static-surface notes); keep the `oapi-codegen` drift gate green.
- [ ] **Step 5: `docs/legal/OPERATOR_CHECKLIST.md`** — first-run runbook (wizard / `novactl setup` / manual-skip; per-TLS-mode guidance; secrets-volume backup; sentinel re-arming).
- [ ] **Step 6: `README.md`** — replace the production-unsupported note with the `docker compose --profile setup up` quickstart pointer (full quickstart = M14).
- [ ] **Step 7: `nginx/nova.conf.example`** — convert the single-origin reference into a two-vhost (public_host + admin_host) reference consistent with the wizard template (it is the documentation companion to `internal/setup/templates/nova.conf.tmpl`).
- [ ] **Step 8: Commit** (`docs(m13): roadmap, master-plan, threat-model, openapi note, operator runbook, README, nginx reference (m13)`).

---

## Final verification (before tagging)

- [ ] `go build ./...` green; `gofmt -l` clean on touched Go files.
- [ ] `go test ./internal/setup/... ./internal/api/handlers/ ./cmd/... -v` → PASS.
- [ ] `go test ./internal/integration/ -run M13 -v` → PASS; `go test -short ./...` green.
- [ ] `make setup-build && scripts/hermetic-spa.sh web/setup/dist` → PASS; `make admin-build widget-build` still green; `npm ci` clean.
- [ ] `docker build -f docker/Dockerfile -t nova-coordinator:dev .` → image builds; `docker compose --profile setup config` valid.
- [ ] All six doc reconciliations landed; the `m13-setup-wizard` deferrals recorded.
- [ ] Spec exit criteria 1–6 each map to a passing test or a human-action-checklist item.
- [ ] Per Bug's milestone workflow: fast-forward merge `m13-setup-wizard` → `main` locally + annotated tag `m13-setup-wizard`; **no remote push.**

---

## Self-review notes

- **Spec coverage:** wizard core (T1–T4), `/setup` seam (T5), config reconciliation (T6), setup-mode boot + two-vhost wiring (T7), headless CLI (T8), web wizard (T9–T10), Docker/compose/entrypoint/TLS (T3+T11), two-vhost + sentinel proof (T12), all six doc reconciliations + nginx reference (T13). Every spec exit criterion has a task.
- **TLS modes:** dev-self-signed/static/handoff in T3 (unit); http-01 webroot + certbot in T11 (compose); real ACME/dev-cert verification in the human-action checklist.
- **Type consistency:** `Answers`, `Paths`, `UserCreator`/`DBUserCreator`, `RenderOperatorYAML`/`RenderNginx`, `ProvisionTLS`/`TLSResult`, `Commit`, `NewSetup` are defined once (T1–T5) and reused with the same signatures in T7/T8/T12.
- **No placeholders:** load-bearing Go (keygen/answers/render/commit/commit_db) is shown in full; mechanical surfaces (web scaffold, Dockerfile stages, compose, docs) are specified prescriptively against named precedents (`web/admin`, `nova.conf.example`, the M11/M12 plans).
