package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type readyzBody struct {
	Federation struct {
		Ready      bool   `json:"ready"`
		Iface      string `json:"iface"`
		BoundAddr  string `json:"bound_addr"`
		WaitingFor string `json:"waiting_for"`
		Since      string `json:"since"`
	} `json:"federation"`
}

func getReadyz(t *testing.T, fr *federationReadiness) (int, readyzBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	fr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	var got readyzBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("readyz body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, got
}

func TestReadyz_NotReadyBeforeBind(t *testing.T) {
	fr := newFederationReadiness("nebula1")
	code, got := getReadyz(t, fr)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 before bind", code)
	}
	if got.Federation.Ready {
		t.Fatal("ready must be false before bind")
	}
	if got.Federation.WaitingFor != "nebula1" {
		t.Fatalf("waiting_for = %q, want nebula1", got.Federation.WaitingFor)
	}
}

func TestReadyz_ReadyAfterBind(t *testing.T) {
	fr := newFederationReadiness("nebula1")
	fr.SetBound("10.42.0.1:9443", "nebula1")

	code, got := getReadyz(t, fr)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200 after bind", code)
	}
	if !got.Federation.Ready {
		t.Fatal("ready must be true after bind")
	}
	if got.Federation.BoundAddr != "10.42.0.1:9443" {
		t.Fatalf("bound_addr = %q", got.Federation.BoundAddr)
	}
	if got.Federation.Iface != "nebula1" {
		t.Fatalf("iface = %q", got.Federation.Iface)
	}
	if got.Federation.WaitingFor != "" {
		t.Fatalf("waiting_for = %q, want empty once bound", got.Federation.WaitingFor)
	}
}

// TestReadyz_DegradedAfterWaitTimeout pins the D-M7.2-8b contract: a wait
// timeout leaves the process alive and reporting not-ready, never exiting.
func TestReadyz_DegradedAfterWaitTimeout(t *testing.T) {
	fr := newFederationReadiness("nebula1")
	fr.Set(false)

	code, got := getReadyz(t, fr)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503 when degraded", code)
	}
	if got.Federation.Ready {
		t.Fatal("degraded must report ready=false")
	}
}

func TestReadinessObserverFiresOnTransitions(t *testing.T) {
	fr := newFederationReadiness("nebula1")
	var seen []bool
	fr.SetObserver(func(b bool) { seen = append(seen, b) })
	fr.SetBound("10.42.0.1:9443", "nebula1")

	// SetObserver reports the current value, then SetBound reports true.
	if len(seen) != 2 || seen[0] != false || seen[1] != true {
		t.Fatalf("observer calls = %v, want [false true]", seen)
	}
}

func TestReadinessNilReceiverIsSafe(t *testing.T) {
	var fr *federationReadiness
	fr.SetObserver(nil)
	fr.Set(true)
	fr.SetBound("x", "y")
}
