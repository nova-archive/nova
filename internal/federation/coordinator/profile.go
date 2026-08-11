package coordinator

import "github.com/nova-archive/nova/internal/federation/wire"

// Capability profiles (P2-M7.3, D-M7.3-22).
//
// A capability is EITHER required at registration OR route-gated. It cannot be
// both, because the two mean opposite things:
//
//   - REQUIRED — a donor that does not advertise it is refused registration.
//     Reserved for what every donor must do for the control plane to work at
//     all: consume the pin change log, and reconcile by snapshot.
//   - ROUTE-GATED — a donor that does not advertise it stays registered, keeps
//     everything it holds, and is simply not sent work of that kind.
//
// Production wiring used to require blob-transfer/v1 while compat_matrix_test
// asserted the two sets were disjoint — but the test built its own profile and
// never evaluated the production one, so the invariant was unenforced and the
// contradiction shipped. Requiring blob-transfer/v1 also refuses registration
// to an older donor outright, which D-M7.3-21b forbids: an older donor stays
// useful, it is only excluded from roles it cannot perform.
//
// These are the single source both cmd/coordinator and the tests consume.
var (
	// ProductionRequiredCapabilities is what fedcoord.Config.RequiredCapabilities
	// is set to in production. Changing it changes who may register.
	ProductionRequiredCapabilities = []string{
		wire.CapPinChangeLog,
		wire.CapSnapshot,
	}

	// RouteGatedCapabilities gate work, not registration. Each is enforced at
	// the point work is created — see the assignment-path filters below — never
	// at the door.
	RouteGatedCapabilities = []string{
		wire.CapBlobTransfer,
		wire.CapReadSource,
		wire.CapRepairStream,
		wire.CapAuditBlockHash,
	}
)
