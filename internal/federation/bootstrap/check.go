// Package bootstrap owns federation bootstrap, adoption and diagnosis:
// `novactl federation init`, `federation doctor`, and `node invite`.
//
// It is operator-only and must never be reachable from the donor graph.
package bootstrap

import (
	"encoding/json"
	"io"
)

// Check is one diagnostic result. Every plane of `federation doctor` produces
// these, and checks REPORT rather than erroring so a single failure does not
// mask the others — an operator debugging a broken bootstrap needs the whole
// picture, not the first problem.
//
// Plane A runs in nova-admin over its own mounts. Plane B runs in nova-doctor,
// which shares the coordinator's network namespace. Plane C is a static check
// over the rendered compose definition. See D-M7.2-3.
type Check struct {
	Plane  string `json:"plane"` // "A" (local), "B" (live), "C" (compose policy)
	ID     string `json:"check_id"`
	Status string `json:"status"` // "pass" | "fail" | "skip"
	Detail string `json:"detail,omitempty"`
}

// Pass builds a passing check.
func Pass(plane, id, detail string) Check {
	return Check{Plane: plane, ID: id, Status: "pass", Detail: detail}
}

// Fail builds a failing check.
func Fail(plane, id, detail string) Check {
	return Check{Plane: plane, ID: id, Status: "fail", Detail: detail}
}

// Skip builds a skipped check (not applicable in this configuration).
func Skip(plane, id, detail string) Check {
	return Check{Plane: plane, ID: id, Status: "skip", Detail: detail}
}

// OK reports whether no check failed. Skips do not count as failures.
func OK(checks []Check) bool {
	for _, c := range checks {
		if c.Status == "fail" {
			return false
		}
	}
	return true
}

// Failures returns only the failing checks.
func Failures(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Status == "fail" {
			out = append(out, c)
		}
	}
	return out
}

// EmitJSON writes checks as a JSON array, for support automation.
func EmitJSON(w io.Writer, checks []Check) error {
	if checks == nil {
		checks = []Check{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(checks)
}
