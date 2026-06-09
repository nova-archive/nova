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
	if strings.Count(conf, "server {") < 3 { // public 443 + admin 8445 + http redirect
		t.Fatalf("expected >=3 server blocks (public/admin/redirect):\n%s", conf)
	}
	mustContain(t, conf, "server_name "+a.Hostname)
	mustContain(t, conf, "location /api/v1/admin")
	mustContain(t, conf, "location /api/v1/auth")
	mustContain(t, conf, "location /widget")
	mustContain(t, conf, "location /fed/")
}

func mustContain(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("missing %q in:\n%s", needle, hay)
	}
}
