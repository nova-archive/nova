package main

import (
	"crypto/x509"
	"embed"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"github.com/google/uuid"
	"github.com/nova-archive/nova/internal/federation/ca"
	"github.com/nova-archive/nova/internal/federation/transport"
)

//go:embed templates/*.tmpl
var nodeTemplates embed.FS

// cmdNode dispatches `novactl node <subcommand>`. The DB-direct commands
// (revoke/rotate-cert/list) are added in a later task.
func cmdNode(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: novactl node <ca-init|issue|issue-coordinator-client|revoke|rotate-cert|list|set-domain|nebula-template>")
	}
	switch args[0] {
	case "ca-init":
		return cmdNodeCAInit(args[1:])
	case "issue":
		return cmdNodeIssue(args[1:])
	case "issue-coordinator-client":
		return cmdNodeIssueCoordinatorClient(args[1:])
	case "revoke":
		return cmdNodeRevoke(args[1:])
	case "rotate-cert":
		return cmdNodeRotateCert(args[1:])
	case "list":
		return cmdNodeList(args[1:])
	case "set-domain":
		return cmdNodeSetDomain(args[1:])
	case "nebula-template":
		return cmdNodeNebulaTemplate(args[1:])
	default:
		return fmt.Errorf("novactl node: unknown subcommand %q", args[0])
	}
}

func writeNodeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func parseFirstCert(pemBytes []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// leafFingerprint computes "sha256:<hex DER>" from a cert PEM.
func leafFingerprint(pemBytes []byte) string {
	c, err := parseFirstCert(pemBytes)
	if err != nil {
		return ""
	}
	return transport.FingerprintDER(c)
}

func cmdNodeCAInit(args []string) error {
	fs := flag.NewFlagSet("node ca-init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "output directory for CA + coordinator server cert")
	coordIP := fs.String("coordinator-ip", "127.0.0.1", "coordinator Nebula overlay IP for the server cert SAN")
	coordDNS := fs.String("coordinator-dns", "localhost", "coordinator DNS SAN")
	if err := fs.Parse(args); err != nil {
		return err
	}
	caCertPEM, caKeyPEM, err := ca.GenerateCA()
	if err != nil {
		return err
	}
	srvPEM, srvKeyPEM, err := ca.IssueServerCert(caCertPEM, caKeyPEM, ca.ServerCertOptions{
		DNSNames:    []string{*coordDNS},
		IPAddresses: []string{*coordIP},
	})
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"federation-ca.crt":           caCertPEM,
		"federation-ca.key":           caKeyPEM,
		"coordinator-federation.crt":  srvPEM,
		"coordinator-federation.key":  srvKeyPEM,
		"federation-ca.manifest.json": []byte(fmt.Sprintf("{\n  \"ca_fingerprint\": %q\n}\n", leafFingerprint(caCertPEM))),
	}
	for name, data := range files {
		perm := os.FileMode(0o644)
		if filepath.Ext(name) == ".key" {
			perm = 0o600
		}
		if err := writeNodeFile(filepath.Join(*dir, name), data, perm); err != nil {
			return err
		}
	}
	fmt.Printf("federation CA + coordinator server cert written to %s\n", *dir)
	return nil
}

func cmdNodeIssue(args []string) error {
	fs := flag.NewFlagSet("node issue", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory holding federation-ca.crt + federation-ca.key")
	name := fs.String("name", "", "donor display name (required)")
	out := fs.String("out", "", "output dir for the donor bundle (default ./<name>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("node issue: --name is required")
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(".", *name)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.crt"))
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.key"))
	if err != nil {
		return err
	}
	nodeID := uuid.New()
	certPEM, keyPEM, err := ca.IssueClientCert(caCertPEM, caKeyPEM, nodeID, *name)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"federation.crt":    certPEM,
		"federation.key":    keyPEM,
		"federation-ca.crt": caCertPEM,
		"node-manifest.json": []byte(fmt.Sprintf("{\n  \"node_id\": %q,\n  \"display_name\": %q,\n  \"fingerprint\": %q\n}\n",
			nodeID.String(), *name, leafFingerprint(certPEM))),
	}
	for fn, data := range files {
		perm := os.FileMode(0o644)
		if filepath.Ext(fn) == ".key" {
			perm = 0o600
		}
		if err := writeNodeFile(filepath.Join(outDir, fn), data, perm); err != nil {
			return err
		}
	}
	fmt.Printf("issued donor cert for %s\n  node_id:     %s\n  fingerprint: %s\n  bundle:      %s\n",
		*name, nodeID.String(), leafFingerprint(certPEM), outDir)
	return nil
}

// cmdNodeIssueCoordinatorClient issues a coordinator federation client cert
// (nova://coordinator/<uuid> URI SAN) for use when the coordinator pulls blobs
// from donor read-source endpoints (P2-M4.1). The UUID is fresh per issuance.
// No nodes row is created — this identity is coordinator-only, not a donor.
//
// Usage: novactl node issue-coordinator-client [--dir CA_DIR] [--out OUT_DIR]
func cmdNodeIssueCoordinatorClient(args []string) error {
	fs := flag.NewFlagSet("node issue-coordinator-client", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory holding federation-ca.crt + federation-ca.key")
	out := fs.String("out", "", "output dir for the coordinator client bundle (default ./coordinator-client)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(".", "coordinator-client")
	}
	caCertPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.crt"))
	if err != nil {
		return err
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(*dir, "federation-ca.key"))
	if err != nil {
		return err
	}
	coordID := uuid.New()
	certPEM, keyPEM, err := ca.IssueCoordinatorClientCert(caCertPEM, caKeyPEM, coordID)
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"federation-client.crt": certPEM,
		"federation-client.key": keyPEM,
		"federation-ca.crt":     caCertPEM,
		"coordinator-client-manifest.json": []byte(fmt.Sprintf(
			"{\n  \"coordinator_id\": %q,\n  \"fingerprint\": %q\n}\n",
			coordID.String(), leafFingerprint(certPEM))),
	}
	for fn, data := range files {
		perm := os.FileMode(0o644)
		if filepath.Ext(fn) == ".key" {
			perm = 0o600
		}
		if err := writeNodeFile(filepath.Join(outDir, fn), data, perm); err != nil {
			return err
		}
	}
	fmt.Printf("issued coordinator client cert\n  coordinator_id: %s\n  fingerprint:    %s\n  bundle:         %s\n",
		coordID.String(), leafFingerprint(certPEM), outDir)
	return nil
}

func cmdNodeNebulaTemplate(args []string) error {
	fs := flag.NewFlagSet("node nebula-template", flag.ContinueOnError)
	name := fs.String("name", "donor", "donor name")
	nebulaIP := fs.String("nebula-ip", "10.42.0.10/24", "donor Nebula overlay IP/CIDR")
	out := fs.String("out", ".", "output dir")
	lhOverlay := fs.String("lighthouse-overlay-ip", "10.42.0.1", "lighthouse overlay IP")
	lhPublic := fs.String("lighthouse-public-ip", "REPLACE_ME", "lighthouse public IP")
	coordOverlay := fs.String("coordinator-overlay-ip", "10.42.0.1", "coordinator overlay IP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data := map[string]string{
		"Name": *name, "NebulaIP": *nebulaIP,
		"LighthouseOverlayIP": *lhOverlay, "LighthousePublicIP": *lhPublic,
		"CoordinatorOverlayIP": *coordOverlay,
	}
	files := map[string]string{
		"nebula-config.yml":   "templates/nebula-config.yml.tmpl",
		"node.yaml":           "templates/node.yaml.tmpl",
		"compose.yaml":        "templates/donor-compose.yaml.tmpl",
		"README.operator.txt": "templates/operator-README.txt.tmpl",
	}
	for outName, tmplPath := range files {
		tmpl, err := template.ParseFS(nodeTemplates, tmplPath)
		if err != nil {
			return err
		}
		f, err := os.Create(filepath.Join(*out, outName))
		if err != nil {
			return err
		}
		if err := tmpl.Execute(f, data); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	fmt.Printf("Nebula + donor templates written to %s (run nebula-cert yourself — see README.operator.txt)\n", *out)
	return nil
}
