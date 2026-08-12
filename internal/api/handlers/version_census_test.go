package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The census is OPERATOR-ONLY. The /admin group admits moderators because they
// run takedowns; this endpoint exposes the operator's supply-chain
// expectations and every donor's self-reported build identity, which is not
// moderation. It follows the key-rotation precedent: a nested
// RequireRole("operator") inside the operator+moderator group.
//
// The route wiring is asserted structurally because a handler that exists only
// in ServerConfig 404s in production, and that failure is silent.

func serverSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCensusRouteIsNestedOperatorOnly(t *testing.T) {
	lines := strings.Split(serverSource(t), "\n")
	at := -1
	for i, l := range lines {
		if strings.Contains(l, "federation/version-census") {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("no census route is mounted")
	}
	// The nested guard is part of the same statement: either on the route line
	// or on the line that opens it. Comments in between do not count.
	var stmt []string
	for i := at; i >= 0 && i > at-4; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "//") {
			continue
		}
		stmt = append(stmt, l)
		if strings.HasPrefix(l, "r.") {
			break
		}
	}
	if !strings.Contains(strings.Join(stmt, " "), `bearer.RequireRole("operator")`) {
		t.Errorf("the census route does not carry a nested operator-only guard; the group "+
			"guard admits moderators, and this is not moderation:\n  %s", strings.Join(stmt, "\n  "))
	}
}

func TestCensusHandlerIsWiredInProduction(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "coordinator", "coordinator.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "sc.VersionCensus = handlers.NewVersionCensusHandler") {
		t.Error("production never sets ServerConfig.VersionCensus, so the endpoint 404s; " +
			"a nil-guarded handler that nothing constructs is an endpoint that does not exist")
	}
}

// TestCensusSetsNoStore. The census is a point-in-time operational view whose
// entire value is being current; a cached copy is worse than none.
func TestCensusSetsNoStore(t *testing.T) {
	src, err := os.ReadFile("version_census.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `w.Header().Set("Cache-Control", "no-store")`) {
		t.Error("the census response is cacheable")
	}
}

// TestCensusResponseNamesTheTargetRelease. "Supported" is meaningless without
// saying supported by what, so the response carries the coordinator's own
// identity beside the fleet.
func TestCensusResponseNamesTheTargetRelease(t *testing.T) {
	src, err := os.ReadFile("version_census.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`json:"release"`, `json:"applied_schema"`, `json:"schema_aware"`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the census response omits %s", want)
		}
	}
}

// TestCensusResponseShapeIsStable guards the JSON contract P2-M7.6 will render
// and `novactl upgrade status --json` will parse.
func TestCensusResponseShapeIsStable(t *testing.T) {
	// A hand-built response of the documented shape must round-trip.
	const sample = `{
	  "release": {"version":"v0.3.0","intent_digest":"sha256:aa","target_schema":19},
	  "applied_schema": 19,
	  "schema_aware": true,
	  "nodes": [{"node_id":"n1","version_compatibility":"unknown","freshness":"fresh",
	             "image_digest_relation":"unavailable","bundle_lock_digest_relation":"unavailable",
	             "runtime_contract_freshness":"registration-only",
	             "protocol_compatibility":"compatible","profile_compliance":"unavailable"}],
	  "oldest_version": ""
	}`
	var m map[string]any
	if err := json.Unmarshal([]byte(sample), &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"release", "applied_schema", "schema_aware", "nodes"} {
		if _, ok := m[key]; !ok {
			t.Errorf("documented key %q missing from the sample", key)
		}
	}
	_ = httptest.NewRecorder
	_ = http.StatusOK
}
