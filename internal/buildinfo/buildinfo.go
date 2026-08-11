// Package buildinfo is the single place a Nova binary learns what it is
// (P2-M7.3, D-M7.3-6a / P0-c).
//
// Before this package the coordinator carried its own `main.buildVersion`, the
// donor binary carried no version symbol at all, and the Makefile computed a
// GO_LDFLAGS value that no build target ever used. Every containerized
// coordinator reported {"version":"dev"}, which is what made the fleet census
// unanswerable: the coordinator could not name itself, so it could not
// meaningfully ask a donor to.
//
// # Stamping
//
// The three values are set at link time:
//
//	-X github.com/nova-archive/nova/internal/buildinfo.version=v0.3.0
//	-X github.com/nova-archive/nova/internal/buildinfo.revision=143c459
//	-X github.com/nova-archive/nova/internal/buildinfo.buildDate=2026-08-11T00:00:00Z
//
// The Makefile's $(GO_LDFLAGS) carries all three, and every build target uses
// it. The Dockerfiles take NOVA_VERSION / NOVA_REVISION / NOVA_BUILD_DATE as
// build args and pass them through to the same flags, so an image and the
// binaries inside it cannot disagree.
//
// # No environment override
//
// There is deliberately no NOVA_VERSION fallback. The coordinator used to have
// one, and it was a fallback that outranked the stamp — an env var that could
// lie about immutable build information. Once a binary is stamped, no
// legitimate caller needs to say otherwise, and the census must not be able to
// launder a claim through one. An unstamped binary says so honestly instead.
package buildinfo

// Defaults describe an unstamped build — `go run`, `go test`, a bare
// `go build`. They are deliberately not empty strings: a census that reads
// "dev" knows it is looking at a developer build, while "" is indistinguishable
// from a field that was never populated.
var (
	version   = "dev"
	revision  = "unknown"
	buildDate = "unknown"
)

// Version is the product version this binary was stamped with, or "dev".
func Version() string { return version }

// Revision is the source commit this binary was built from, or "unknown".
func Revision() string { return revision }

// BuildDate is the RFC 3339 build timestamp, or "unknown".
func BuildDate() string { return buildDate }

// Stamped reports whether this binary carries real build information. A census
// axis reads "unknown" rather than "unsupported" when this is false.
func Stamped() bool { return version != "dev" }

// String renders the one-line form used by --version and structured logs.
func String() string {
	return version + " (" + revision + ", built " + buildDate + ")"
}
