// Package notify is the operator-side event emitter seam for federation signals
// (node_revoked, degraded, shrinking, concentrated/homogeneous). Task 3 defines
// the interface + NoopNotifier so the liveness sweeper can emit before Task 7's
// best-effort HTTP dispatcher exists. MUST stay off the donor (cmd/node) build
// graph — it is operator-only.
package notify

import "context"

// Event is a federation signal. ScopeKey scopes once-per-window suppression so
// distinct subjects (e.g. two revoked nodes) are not collapsed into one
// suppressed event (D-M5-9a).
type Event struct {
	Type     string
	ScopeKey string
	Payload  map[string]any
}

// Notifier emits federation events. Implementations are best-effort: Emit must
// never block on or fail a caller's transaction, and is called AFTER commit
// (D-M5-9).
type Notifier interface {
	Emit(ctx context.Context, ev Event)
}

// NoopNotifier discards events. Used where signalling is unconfigured and in
// tests that do not assert on emission.
type NoopNotifier struct{}

// Emit discards the event.
func (NoopNotifier) Emit(context.Context, Event) {}
