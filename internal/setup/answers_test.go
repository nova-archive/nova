// internal/setup/answers_test.go
package setup

import (
	"testing"

	"gopkg.in/yaml.v3"
)

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

func TestAnswers_YAMLConstituentsParse(t *testing.T) {
	const doc = `
hostname: img.example.com
contact_email: abuse@example.com
admin_email: op@example.com
admin_password: correct horse battery
tls_mode: dev-self-signed
auth_mode: local
record_source_ip: false
source_ip_retention_days: 1
public_ipfs_dht: false
paranoid: true
`
	var a Answers
	if err := yaml.Unmarshal([]byte(doc), &a); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if a.RecordSourceIP == nil || *a.RecordSourceIP {
		t.Fatalf("record_source_ip: want explicit false, got %v", a.RecordSourceIP)
	}
	if a.SourceIPRetentionDays != 1 {
		t.Fatalf("source_ip_retention_days: want 1, got %d", a.SourceIPRetentionDays)
	}
	if a.PublicIPFSDHT {
		t.Fatal("public_ipfs_dht: want false")
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
		"negative retention":    func(a *Answers) { a.SourceIPRetentionDays = -1 },
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
