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
// round-tripping through the real loader. An un-loadable render is a bug.
func RenderOperatorYAML(a Answers) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	cfg := config.Config{
		Operator: config.Operator{
			Hostname:     a.Hostname,
			ContactEmail: a.ContactEmail,
			DisplayName:  a.DisplayName,
		},
		TLS: config.TLS{
			Mode:     a.TLSMode,
			CertPath: a.CertPath,
			KeyPath:  a.KeyPath,
		},
		Auth: config.Auth{
			IssuerURL: a.IssuerURL,
			ClientID:  a.ClientID,
			Paranoid:  a.Paranoid,
		},
		Moderation: config.Moderation{
			TakedownDefaultAction: "quarantine",
		},
		Orchestrator: config.Orchestrator{
			Replication: config.Replication{
				Factor: config.ReplicationFactor{
					Important: config.DefaultReplicationImportant,
					Normal:    config.DefaultReplicationNormal,
					Cache:     config.DefaultReplicationCache,
				},
			},
		},
		Coordinator: config.Coordinator{
			PublicIpfsDht:  a.PublicIPFSDHT,
			RecordSourceIP: a.RecordSourceIP,
		},
		Uploads: config.Uploads{
			PublicUploads: a.PublicUploads,
		},
		SourceIPRetentionDays: a.SourceIPRetentionDays,
		TosURL:                a.TosURL,
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
	Upstream    string
	CertPath    string
	KeyPath     string
	AdminListen string
	ACME        bool
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
	default: // dev-self-signed, http-01, dns-01, onion
		v.CertPath = "/etc/nova/tls/fullchain.pem"
		v.KeyPath = "/etc/nova/tls/privkey.pem"
	}
	var buf bytes.Buffer
	if err := nginxTmpl.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("setup: render nginx: %w", err)
	}
	return buf.String(), nil
}
