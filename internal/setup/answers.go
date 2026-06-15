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

	AuthMode      string `json:"auth_mode" yaml:"auth_mode"` // local|external
	IssuerURL     string `json:"issuer_url,omitempty" yaml:"issuer_url,omitempty"`
	ClientID      string `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	PublicUploads bool   `json:"public_uploads" yaml:"public_uploads"`
	TosURL        string `json:"tos_url,omitempty" yaml:"tos_url,omitempty"`
	Paranoid      bool   `json:"paranoid" yaml:"paranoid"`

	// Privacy-preset constituents (M0.5). All optional: an omitted field keeps
	// today's behavior — ApplyPrivacyPreset fills it from the paranoid preset.
	// Real yaml tags (not `-`): a headless operator sets these in the answers
	// file that `novactl setup` reads via yaml.Unmarshal (cmd/novactl/main.go:
	// loadAnswersFile), matching every other Answers field.
	RecordSourceIP        *bool `json:"record_source_ip,omitempty" yaml:"record_source_ip,omitempty"`
	SourceIPRetentionDays int   `json:"source_ip_retention_days,omitempty" yaml:"source_ip_retention_days,omitempty"`
	PublicIPFSDHT         bool  `json:"public_ipfs_dht,omitempty" yaml:"public_ipfs_dht,omitempty"`
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
	if a.SourceIPRetentionDays < 0 {
		return fmt.Errorf("setup: source_ip_retention_days must be >= 0")
	}
	return nil
}
