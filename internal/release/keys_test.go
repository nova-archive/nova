package release

import (
	"slices"
	"strings"
	"testing"
)

// TestKnownConfigKeysComeFromTheStruct. A hand-maintained list would be a
// second source of truth about the config surface, and the first thing to rot.
func TestKnownConfigKeysComeFromTheStruct(t *testing.T) {
	keys := KnownConfigKeys()

	for _, want := range []string{
		"operator", "operator.hostname", "operator.contact_email",
		"auth.issuer_url", "auth.paranoid",
		"federation", "orchestrator.tick_interval_seconds",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("%q is a real config key and reflection did not find it", want)
		}
	}
	if slices.Contains(keys, "operator.Hostname") {
		t.Error("keys are the YAML names, not the Go field names")
	}
	if !slices.IsSorted(keys) {
		t.Error("keys must be sorted; an unstable order makes diffs unreadable")
	}
}

// TestInspectConfigKeysWarnsAndNeverFails. `config/operator_yaml.go` parses with
// plain yaml.Unmarshal and no KnownFields, so an unknown key is silently ignored
// today. Turning that into a hard error in the same release that introduces the
// check would break deployments during an upgrade for a misspelling that has
// been harmless for months.
func TestInspectConfigKeysWarnsAndNeverFails(t *testing.T) {
	src := []byte(`
operator:
  hostname: nova.example
  contact_emial: bug@example
auth:
  issuer_url: https://issuer.example
tls_mode: strict
`)
	findings, err := InspectConfigKeys(src)
	if err != nil {
		t.Fatalf("an unknown key must never be an error: %v", err)
	}

	got := map[string]bool{}
	for _, f := range findings {
		got[f.Path] = true
		if f.Kind != "unknown" {
			t.Errorf("%s: kind = %q", f.Path, f.Kind)
		}
		if !strings.Contains(f.Detail, "IGNORED") {
			t.Errorf("%s: the warning must say what actually happens to the key: %q", f.Path, f.Detail)
		}
	}
	for _, want := range []string{"operator.contact_emial", "tls_mode"} {
		if !got[want] {
			t.Errorf("%q was not reported", want)
		}
	}
	for _, unwanted := range []string{"operator", "operator.hostname", "auth", "auth.issuer_url"} {
		if got[unwanted] {
			t.Errorf("%q is a real key and must not be reported", unwanted)
		}
	}
}

// TestInspectConfigKeysDoesNotDescendIntoAnUnknownKey. Everything under an
// unknown key is unknown too, and listing each leaf buries the one line that
// matters.
func TestInspectConfigKeysDoesNotDescendIntoAnUnknownKey(t *testing.T) {
	findings, err := InspectConfigKeys([]byte(`
feddration:
  listen_addr: ":8443"
  ca:
    path: /etc/nova/ca.pem
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != "feddration" {
		t.Fatalf("findings = %+v, want exactly the misspelled root", findings)
	}
}

// TestInspectConfigKeysAcceptsAKeyFromANewerRelease. Preflight runs from the
// TARGET binary against the RUNNING deployment's file, which in a rollback
// rehearsal can carry keys a newer release added. It is still only a warning,
// and — the part that matters — the walk never re-marshals, so the value
// survives untouched.
func TestInspectConfigKeysAcceptsAKeyFromANewerRelease(t *testing.T) {
	src := []byte("operator:\n  hostname: nova.example\n  future_setting: 42\n")
	findings, err := InspectConfigKeys(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Path != "operator.future_setting" {
		t.Fatalf("findings = %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "left untouched") {
		t.Errorf("the warning must promise preservation: %q", findings[0].Detail)
	}
}

// TestInspectConfigKeysFailsOnAFileItCannotRead. A file the binary cannot parse
// is a real blocker, not a warning: nothing downstream can act on it.
func TestInspectConfigKeysFailsOnAFileItCannotRead(t *testing.T) {
	if _, err := InspectConfigKeys([]byte("operator:\n\thostname: x\n  bad")); err == nil {
		t.Fatal("malformed YAML must be an error")
	}
	if f, err := InspectConfigKeys(nil); err != nil || len(f) != 0 {
		t.Fatalf("an empty file sets nothing, which is legal: %v %+v", err, f)
	}
}
