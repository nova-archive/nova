package main

import (
	"strings"
	"testing"
)

func TestComposePolicy_RejectsFedpkiOnLongRunningService(t *testing.T) {
	in := []byte(`
services:
  coordinator:
    image: x@sha256:a
    volumes:
      - {type: volume, source: nova-fedpki, target: /pki}
  nova-admin:
    image: y@sha256:b
    volumes:
      - {type: volume, source: nova-fedpki, target: /pki}
`)
	res := checkComposePolicy(in)
	if res.OK() {
		t.Fatal("coordinator mounting nova-fedpki must be a custody violation")
	}
	if !res.Has("custody.fedpki") {
		t.Fatalf("want custody.fedpki, got %v", res.Violations())
	}
	// The admin service mounting it is legitimate and must NOT be reported.
	for _, v := range res.Violations() {
		if strings.Contains(v.Detail, `"nova-admin"`) {
			t.Errorf("nova-admin holding issuance authority is the design, not a violation: %s", v.Detail)
		}
	}
}

func TestComposePolicy_AcceptsAdminOnlyFedpki(t *testing.T) {
	in := []byte(`
services:
  coordinator:
    image: x@sha256:a
  nebula:
    image: n@sha256:c
  nova-admin:
    image: y@sha256:b
    volumes:
      - {type: volume, source: nova-fedpki, target: /pki}
      - {type: bind, source: ./import, target: /import, read_only: true}
`)
	if res := checkComposePolicy(in); !res.OK() {
		t.Fatalf("admin-only fedpki must pass, got %v", res.Violations())
	}
}

func TestComposePolicy_RejectsWritableImport(t *testing.T) {
	in := []byte(`
services:
  nova-admin:
    image: y@sha256:b
    volumes:
      - {type: bind, source: ./import, target: /import, read_only: false}
`)
	res := checkComposePolicy(in)
	if !res.Has("custody.import") {
		t.Fatalf("writable /import must violate custody.import, got %v", res.Violations())
	}
}

func TestComposePolicy_RejectsImportOnNonAdminService(t *testing.T) {
	in := []byte(`
services:
  nova-doctor:
    image: y@sha256:b
    volumes:
      - {type: bind, source: ./import, target: /import, read_only: true}
`)
	res := checkComposePolicy(in)
	if !res.Has("custody.import") {
		t.Fatalf("only nova-admin may read the adoption import, got %v", res.Violations())
	}
}

// TestComposePolicy_RejectsCAKeyByPath covers the bypass where a service does
// not mount the volume but bind-mounts the key file directly.
func TestComposePolicy_RejectsCAKeyByPath(t *testing.T) {
	in := []byte(`
services:
  coordinator:
    image: x@sha256:a
    volumes:
      - ./pki/federation-ca.key:/run/secrets/federation-ca.key:ro
`)
	res := checkComposePolicy(in)
	if !res.Has("custody.secrets") {
		t.Fatalf("mounting a CA private key by path must violate custody.secrets, got %v", res.Violations())
	}
}

// TestComposePolicy_ShortFormVolumeSyntax: compose accepts both spellings, so
// the gate must too, or a violation hides behind a syntax choice.
func TestComposePolicy_ShortFormVolumeSyntax(t *testing.T) {
	in := []byte(`
services:
  coordinator:
    image: x@sha256:a
    volumes:
      - nova-fedpki:/pki:ro
`)
	res := checkComposePolicy(in)
	if !res.Has("custody.fedpki") {
		t.Fatalf("short-form mounts must be checked too, got %v", res.Violations())
	}
}

func TestComposePolicy_ShortFormReadOnlyImportIsAccepted(t *testing.T) {
	in := []byte(`
services:
  nova-admin:
    image: y@sha256:b
    volumes:
      - ./import:/import:ro
`)
	if res := checkComposePolicy(in); !res.OK() {
		t.Fatalf("short-form read-only import must pass, got %v", res.Violations())
	}
}

func TestComposePolicy_RejectsUnparseableInput(t *testing.T) {
	res := checkComposePolicy([]byte("this: is: not: compose\n"))
	if res.OK() {
		t.Fatal("unparseable input must fail rather than silently pass")
	}
}

func TestComposePolicy_RejectsEmptyServices(t *testing.T) {
	res := checkComposePolicy([]byte("version: '3'\n"))
	if res.OK() {
		t.Fatal("a config with no services must fail — it proves nothing")
	}
}
