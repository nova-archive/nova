package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ReadyCheck is one named liveness probe executed by ReadyHandler. Fn must
// return nil when its dependency is healthy and an error otherwise. The
// handler runs all checks in parallel under a single deadline so a single
// slow dep does not stretch the overall probe time.
type ReadyCheck struct {
	Name string
	Fn   func(ctx context.Context) error
}

// ReadyHandler serves /readyz. It runs each registered check in parallel
// with the configured timeout and aggregates the outcomes; the response
// is 200 only when every check succeeds, 503 otherwise. M6.2 D1.
type ReadyHandler struct {
	checks  []ReadyCheck
	timeout time.Duration
}

// NewReadyHandler builds the handler. timeout <= 0 defaults to 1 second
// (the same bound Kubernetes uses for readinessProbe.timeoutSeconds).
func NewReadyHandler(timeout time.Duration, checks ...ReadyCheck) *ReadyHandler {
	if timeout <= 0 {
		timeout = time.Second
	}
	// Defensive copy so the caller can mutate their slice without
	// affecting handler state.
	c := make([]ReadyCheck, len(checks))
	copy(c, checks)
	return &ReadyHandler{checks: c, timeout: timeout}
}

// readyResult is one row in the /readyz JSON payload.
type readyResult struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latency_ms"`
	Err       string `json:"err,omitempty"`
}

// Serve is the chi-compatible handler. With zero registered checks the
// handler returns 200 with an empty checks slice (matches /health's
// "process alive" semantics when readiness is not yet configured).
func (h *ReadyHandler) Serve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	results := make([]readyResult, len(h.checks))
	var wg sync.WaitGroup
	for i, c := range h.checks {
		i, c := i, c
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := c.Fn(ctx)
			results[i] = readyResult{
				Name:      c.Name,
				OK:        err == nil,
				LatencyMs: time.Since(start).Milliseconds(),
			}
			if err != nil {
				results[i].Err = err.Error()
			}
		}()
	}
	wg.Wait()

	allOK := true
	for _, r := range results {
		if !r.OK {
			allOK = false
			break
		}
	}
	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     allOK,
		"checks": results,
	})
}
