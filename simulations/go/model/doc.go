//go:build novasim

// Package model is a discrete-event resilience model for a Nova federation.
//
// It is the scaled half of the "calibrated hybrid" simulation: the calib
// package measures real per-operation costs from the production
// internal/envelope and internal/ipfs primitives, and this package consumes
// those constants to explore federation behaviour at thousands-of-nodes
// scale without pushing real bytes.
//
// The whole simulations/go tree is gated behind the `novasim` build tag so
// the default `go build ./...` / CI surface (and the P2-M0-gated
// internal/orchestrator package, which this deliberately does NOT touch)
// are unaffected. Build and test with:
//
//	go build -tags novasim ./simulations/go/...
//	go test  -tags novasim ./simulations/go/model/...
//
// Units. Everything internal to this package is explicit bytes and seconds.
// Profiles express daily egress budget in bytes and link speed in decimal
// megabits/sec. This is intentionally more unit-consistent than the original
// simulations/orchestrator_resilience.py, which mixes a MiB-based daily
// budget with a decimal-MB/s link conversion; cross-validation against the
// Python sim is therefore "within noise", not digit-identical. See the Go
// suite README.
package model
