package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// federationReadiness is the coordinator-authoritative answer to "is the
// federation listener up, and on what?" (P2-M7.2 D-M7.2-8c).
//
// It is served on the operator-only metrics listener, never the public vhost:
// /health is pure liveness by contract, and the overlay address is topology the
// public surface must not disclose. `federation doctor --live` (Plane B) reads
// this rather than probing from outside, because this process is the only thing
// that actually knows what it bound.
type federationReadiness struct {
	mu         sync.RWMutex
	ready      bool
	iface      string
	boundAddr  string
	waitingFor string
	since      time.Time
	onChange   func(bool)
}

// newFederationReadiness starts in the not-ready state, waiting on iface.
func newFederationReadiness(iface string) *federationReadiness {
	return &federationReadiness{
		iface:      iface,
		waitingFor: iface,
		since:      time.Now().UTC(),
	}
}

// SetObserver wires the nova_federation_listener_ready gauge. Nil-safe, matching
// the observer-seam pattern the other P2-M7 metric families use so consuming
// packages stay prometheus-free.
func (f *federationReadiness) SetObserver(fn func(bool)) {
	if f == nil {
		return
	}
	f.onChange = fn
	f.mu.RLock()
	ready := f.ready
	f.mu.RUnlock()
	if fn != nil {
		fn(ready)
	}
}

// Set records a readiness transition without a bind (the degraded path).
func (f *federationReadiness) Set(ready bool) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.ready, f.since = ready, time.Now().UTC()
	f.mu.Unlock()
	if f.onChange != nil {
		f.onChange(ready)
	}
}

// SetBound records a successful bind: ready, on this address and interface.
func (f *federationReadiness) SetBound(addr, iface string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.ready, f.boundAddr, f.waitingFor, f.since = true, addr, "", time.Now().UTC()
	if iface != "" {
		f.iface = iface
	}
	f.mu.Unlock()
	if f.onChange != nil {
		f.onChange(true)
	}
}

// Handler serves /readyz. 200 when federation is ready, 503 otherwise, with the
// same JSON body either way so a caller can always read the reason.
func (f *federationReadiness) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.RLock()
		body := map[string]any{"federation": map[string]any{
			"ready":       f.ready,
			"iface":       f.iface,
			"bound_addr":  f.boundAddr,
			"waiting_for": f.waitingFor,
			"since":       f.since.Format(time.RFC3339),
		}}
		ready := f.ready
		f.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}
