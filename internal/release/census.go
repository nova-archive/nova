package release

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/mod/semver"
)

// The fleet census (P2-M7.3, D-M7.3-13).
//
// # Orthogonal axes, never one enum
//
// A donor can be current, stale and digest-mismatched at the same time. One
// enum forces a precedence order onto facts that have none, and whichever fact
// loses is the one the operator needed. So each property is its own axis and
// classification happens in Go — SQL lexical grouping cannot compare SemVer,
// and `v0.10.0 < v0.9.0` is exactly the kind of wrong answer that reads as
// authoritative.
//
// # Unknown is not unsupported
//
// Baseline donors report nothing. The interop contract is negotiated protocol
// plus capabilities; a version string was never it. `unknown` therefore blocks
// nothing — not placement, not durability counting, not eviction.
//
// # The read path is schema-version-aware
//
// `upgrade check` runs from the TARGET nova-admin BEFORE migration, against
// schema 18, where none of the 0019 columns exist. A generated query naming
// them fails before preflight can evaluate anything. This is a prerequisite for
// the first-transition scenarios rather than a graceful-degradation nicety.

// Axis values. Strings rather than enums because they are printed, JSON-encoded
// and read by a human more often than they are switched on.
const (
	// Version compatibility.
	VersionCurrent     = "current"
	VersionSupported   = "supported"
	VersionUnsupported = "unsupported"
	VersionUnknown     = "unknown"
	VersionInvalid     = "invalid"

	// Liveness freshness.
	Fresh = "fresh"
	Stale = "stale"

	// Digest relations.
	DigestMatch       = "match"
	DigestMismatch    = "mismatch"
	DigestUnavailable = "unavailable"

	// Runtime-contract freshness.
	ContractRuntime          = "runtime"
	ContractRegistrationOnly = "registration-only"
	ContractUnknown          = "unknown"

	// Protocol and profile.
	Compatible    = "compatible"
	Incompatible  = "incompatible"
	Compliant     = "compliant"
	Nonconforming = "nonconforming"

	// Any axis the current schema cannot answer.
	Unavailable = "unavailable"
)

// NodeCensusRow is one donor's raw census inputs.
type NodeCensusRow struct {
	NodeID      string
	DisplayName string
	Status      string
	TrustState  string

	SelectedProtocol string
	LastSeenAt       time.Time

	// LegacyClientVersion is nodes.client_version, written at registration by
	// donors old and new. It is the only version a pre-contract donor has.
	LegacyClientVersion string

	ReportedVersion          string
	ReportedImageDigest      string
	ReportedBundleLockDigest string
	EffectiveCapabilities    []string
	ReportedProtocols        []string
	ContractObservedAt       *time.Time

	ExpectedImageDigest      string
	ExpectedBundleLockDigest string

	// SchemaAware is false when the row came from the baseline read path, so
	// every 0019-derived axis is reported as unavailable rather than guessed.
	SchemaAware bool
}

// Axes is one donor classified. Every field is independent.
type Axes struct {
	NodeID      string `json:"node_id"`
	DisplayName string `json:"display_name,omitempty"`

	Version          string `json:"version_compatibility"`
	VersionReported  string `json:"version_reported,omitempty"`
	Freshness        string `json:"freshness"`
	ImageDigest      string `json:"image_digest_relation"`
	BundleLockDigest string `json:"bundle_lock_digest_relation"`
	ContractFresh    string `json:"runtime_contract_freshness"`
	Protocol         string `json:"protocol_compatibility"`
	Profile          string `json:"profile_compliance"`

	// Roles reports per-role eligibility from the effective capability set.
	Roles map[string]bool `json:"roles,omitempty"`

	// SupportNote is the human-readable window explanation.
	SupportNote string `json:"support_note,omitempty"`
}

// Classify computes every axis for one donor. It is pure: a catalog, a row, a
// clock and a staleness threshold.
func Classify(cat Catalog, r NodeCensusRow, now time.Time, staleAfter time.Duration) Axes {
	a := Axes{NodeID: r.NodeID, DisplayName: r.DisplayName}

	// Freshness is independent of everything else and available at any schema.
	a.Freshness = Fresh
	if r.LastSeenAt.IsZero() || now.Sub(r.LastSeenAt) > staleAfter {
		a.Freshness = Stale
	}

	// Protocol compatibility: the actual interop contract.
	switch {
	case r.SelectedProtocol == "":
		a.Protocol = VersionUnknown
	case r.SelectedProtocol == "fed/v1":
		a.Protocol = Compatible
	default:
		a.Protocol = Incompatible
	}

	if !r.SchemaAware {
		// The baseline read path. Synthesize rather than guess: an axis the
		// schema cannot answer is `unavailable`, which is visibly different
		// from `unknown` (a donor that told us nothing) and from a real value.
		a.Version = versionAxis(cat, r.LegacyClientVersion, now, &a)
		a.ImageDigest = Unavailable
		a.BundleLockDigest = Unavailable
		a.ContractFresh = Unavailable
		a.Profile = Unavailable
		return a
	}

	reported := r.ReportedVersion
	if reported == "" {
		reported = r.LegacyClientVersion
	}
	a.Version = versionAxis(cat, reported, now, &a)

	a.ImageDigest = digestAxis(r.ExpectedImageDigest, r.ReportedImageDigest)
	a.BundleLockDigest = digestAxis(r.ExpectedBundleLockDigest, r.ReportedBundleLockDigest)

	switch {
	case r.ContractObservedAt == nil:
		// Never sent a contract: a pre-M7.3 donor, judged on its registration
		// snapshot. Not an error, and not the same as a donor that stopped.
		a.ContractFresh = ContractRegistrationOnly
	case r.ReportedVersion == "":
		// Observed before, nothing now: either rolled back, or a contract
		// version this coordinator cannot parse.
		a.ContractFresh = ContractUnknown
	default:
		a.ContractFresh = ContractRuntime
	}

	a.Profile = Compliant
	for _, want := range cat.Profiles.Core {
		if !slices.Contains(r.EffectiveCapabilities, want) {
			a.Profile = Nonconforming
		}
	}
	if len(r.EffectiveCapabilities) == 0 && a.ContractFresh == ContractRegistrationOnly {
		a.Profile = Unavailable
	}

	if len(cat.Profiles.Roles) > 0 {
		a.Roles = map[string]bool{}
		for role, needs := range cat.Profiles.Roles {
			eligible := true
			for _, want := range needs {
				if !slices.Contains(r.EffectiveCapabilities, want) {
					eligible = false
				}
			}
			a.Roles[role] = eligible
		}
	}
	return a
}

func versionAxis(cat Catalog, reported string, now time.Time, a *Axes) string {
	a.VersionReported = reported
	switch {
	case reported == "":
		return VersionUnknown
	case reported == cat.Version:
		return VersionCurrent
	}

	s := SupportedBy(cat, reported, now)
	a.SupportNote = s.Explain()
	switch {
	case !s.Known:
		// Not in the catalog. Malformed is worth distinguishing from merely
		// unrecognized: the first is a donor sending nonsense, the second is a
		// donor older than this release's memory.
		if !semver.IsValid(reported) && !isCommitID(reported) {
			return VersionInvalid
		}
		return VersionUnknown
	case s.Supported:
		return VersionSupported
	default:
		return VersionUnsupported
	}
}

func isCommitID(s string) bool {
	rest, ok := cutPrefix(s, "commit:")
	if !ok {
		return false
	}
	return len(rest) >= 7
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// digestAxis compares what the operator authorized against what the donor
// reports. A mismatch is a SUPPLY-CHAIN WARNING and nothing else: it never
// alters placement, durability or interop.
func digestAxis(expected, reported string) string {
	if expected == "" || reported == "" {
		return DigestUnavailable
	}
	if expected == reported {
		return DigestMatch
	}
	return DigestMismatch
}

// OldestByVersion returns the lowest-precedence reported version in the fleet.
//
// By VERSION, not by last_seen_at. `min(last_seen_at)` answers "who has been
// quiet longest", which is a liveness question wearing a version question's
// clothes.
func OldestByVersion(rows []Axes) string {
	oldest := ""
	for _, r := range rows {
		v := r.VersionReported
		if !semver.IsValid(v) {
			continue
		}
		if oldest == "" || semver.Compare(v, oldest) < 0 {
			oldest = v
		}
	}
	return oldest
}

// ---------------------------------------------------------------------------
// The schema-version-aware read path.
// ---------------------------------------------------------------------------

// SchemaWithCensusColumns is the first schema version carrying the census
// columns. Below it, the baseline path is the only one that can run.
const SchemaWithCensusColumns = 19

// AppliedSchema reads the applied goose version. A database with no goose table
// at all reports 0, which is a fresh install rather than an error.
func AppliedSchema(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var v int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		// The table does not exist: nothing has been applied here.
		return 0, nil
	}
	return v, nil
}

// ReadCensus loads the fleet, choosing the read path by probing the schema
// FIRST.
//
// This is why the probe exists: `upgrade check` runs from the target admin
// before migration, so the columns the full query names are not there yet, and
// a query that names them fails before preflight can evaluate anything.
func ReadCensus(ctx context.Context, pool *pgxpool.Pool) ([]NodeCensusRow, error) {
	schema, err := AppliedSchema(ctx, pool)
	if err != nil {
		return nil, err
	}
	if schema < SchemaWithCensusColumns {
		return readCensusBaseline(ctx, pool)
	}
	return readCensusFull(ctx, pool)
}

func readCensusBaseline(ctx context.Context, pool *pgxpool.Pool) ([]NodeCensusRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, COALESCE(display_name,''), status::text, trust_state,
		       COALESCE(selected_protocol,''), COALESCE(last_seen_at, joined_at),
		       COALESCE(client_version,'')
		FROM nodes ORDER BY joined_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, false)
}

func readCensusFull(ctx context.Context, pool *pgxpool.Pool) ([]NodeCensusRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, COALESCE(display_name,''), status::text, trust_state,
		       COALESCE(selected_protocol,''), COALESCE(last_seen_at, joined_at),
		       COALESCE(client_version,''),
		       COALESCE(reported_client_version,''), COALESCE(reported_image_digest,''),
		       COALESCE(reported_bundle_lock_digest,''),
		       COALESCE(effective_capabilities, ARRAY[]::text[]),
		       COALESCE(reported_protocols, ARRAY[]::text[]),
		       runtime_contract_observed_at,
		       COALESCE(expected_image_digest,''), COALESCE(expected_bundle_lock_digest,'')
		FROM nodes ORDER BY joined_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, true)
}

func collect(rows pgx.Rows, schemaAware bool) ([]NodeCensusRow, error) {
	var out []NodeCensusRow
	for rows.Next() {
		var r NodeCensusRow
		r.SchemaAware = schemaAware
		var err error
		if schemaAware {
			err = rows.Scan(&r.NodeID, &r.DisplayName, &r.Status, &r.TrustState,
				&r.SelectedProtocol, &r.LastSeenAt, &r.LegacyClientVersion,
				&r.ReportedVersion, &r.ReportedImageDigest, &r.ReportedBundleLockDigest,
				&r.EffectiveCapabilities, &r.ReportedProtocols, &r.ContractObservedAt,
				&r.ExpectedImageDigest, &r.ExpectedBundleLockDigest)
		} else {
			err = rows.Scan(&r.NodeID, &r.DisplayName, &r.Status, &r.TrustState,
				&r.SelectedProtocol, &r.LastSeenAt, &r.LegacyClientVersion)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
