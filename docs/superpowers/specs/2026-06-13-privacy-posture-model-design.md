# P2-M0.2 — Privacy-posture model (`paranoid` → preset + warn-not-force)

## Context

Phase 1 ships `auth.paranoid` as an opaque boolean that, when true,
*silently force-mutates* the loaded config (`internal/config/paranoid.go`):
it drops **all** webhook destinations, caps source-IP retention to 1 day, and
(at coordinator startup) disables source-IP recording, refuses public-bound
metrics, and warns on ACME. An operator can't see what it changes, can't keep
one protection while relaxing another, and the only lever to stop source-IP
recording is this all-or-nothing flag.

Bug's first-run onboarding review (the P2-M0.1 friction report, 2026-06-13)
flagged that the wizard never explains these load-bearing choices, so a new
operator can't give informed consent at the moment it matters. This milestone
reframes the **backend model** so the wizard (P2-M0.5) and the admin Settings
modal (P2-M0.6) can later render individual, explained privacy controls. It is
backend + normative-spec only — **no UI**.

## Decisions (ratified with Bug, 2026-06-13)

- **`paranoid` stays DEFAULT-OFF.** Most operators' threat model doesn't warrant
  dropping telemetry, ACME auto-renewal, IP retention, and webhooks; default-on
  would add friction for the common case.
- **Preset, not force.** `paranoid: true` becomes a *named preset* that sets
  protective **defaults** for its constituent settings. Explicit operator values
  WIN; weakening one emits a clear consequence **warning** (startup log) instead
  of being silently overridden or impossible.
- **Constituent settings are individually addressable** so each can be
  exposed and tuned on its own (the wizard/modal hang off this).
- **Legal/safety gates stay HARD fails** (refuse-to-start): `uploads.public_uploads`
  without `tos_url` (T1.20) and `auth.anonymous` without a moderation flow.
  These are liability/abuse gates, not privacy preferences — out of scope for
  warn-not-force.

## The model

`paranoid` is a convenience preset over these settings:

| Setting | Location | Default (paranoid off) | Preset (paranoid on) | Operator override (weaker) |
|---|---|---|---|---|
| Outbound webhooks | `webhooks` (existing) | honored | none added | **kept + WARN** (egress active) |
| Source-IP recording | `coordinator.record_source_ip` (**NEW**, `*bool`) | record (true) | don't record | **kept + WARN** |
| Source-IP retention | `source_ip_retention_days` (existing) | 30 | 1 | **kept + WARN** if higher |
| Public IPFS DHT | `coordinator.public_ipfs_dht` (existing) | private (false) | private | enabling → WARN |
| Metrics/OTel public exposure | startup-enforced (no yaml field yet) | loopback only | refuse to widen | unchanged this milestone |
| ACME auto-renew | `tls.mode` | operator's mode | warn (CT-log exposure) | unchanged (already a warning) |

Key change vs today: webhooks and retention are **no longer force-nuked** — the
preset sets their default and they are *kept-with-warning* when explicitly set.
Source-IP recording becomes its own field; effective value is
`record_source_ip` when set, else `!paranoid` (preserves today's env
`NOVA_PARANOID` behavior and lets an operator stop recording without enabling
the whole preset).

## Implementation

- `internal/config/types.go`: add `Coordinator.RecordSourceIP *bool`
  (`record_source_ip`, tri-state). Add unexported `Config.privacyWarnings
  []string` + `func (c *Config) PrivacyWarnings() []string`.
- `internal/config/paranoid.go`: rename `ApplyParanoid` →
  `ApplyPrivacyPreset(cfg) []string`; warn-not-force per the table; return the
  warnings.
- `internal/config/operator_yaml.go`: `cfg.privacyWarnings =
  ApplyPrivacyPreset(&cfg)` in `LoadFromBytes` (after `validate`).
- `cmd/coordinator/main.go`: derive `recordIP` from the effective
  `record_source_ip` (`explicit ? *v : !paranoid`); log `cfg.PrivacyWarnings()`
  at WARN on startup.
- `cmd/novactl/main.go:1191`: fix the misleading prompt copy ("suppress
  source-IP recording") to describe the full preset.
- Tests: reframe `paranoid_test.go` (webhooks kept + warned; retention kept +
  warned); add `record_source_ip` derivation + warning tests; keep
  `cmd/coordinator/main_test.go` green.

## Normative spec revisions

- `docs/PRIVACY_AUDIT.md` § "`paranoid: true` mode": change matrix language from
  "disabled regardless of config" to **preset-default + override-with-warning**;
  add the `record_source_ip` row; add a "warn, don't force" subsection and the
  hard-fail carve-outs; state default-off.
- `docs/specs/ARCHITECTURE_DECISIONS.md`: amend the `paranoid` row (preset+warn,
  default-off); add `coordinator.record_source_ip`; reaffirm T1.20 stays a hard
  legal gate.
- `docs/THREAT_MODEL.md`: note the fail-open implication as a deliberate
  informed-consent tradeoff (see below).

## Tradeoff flagged

Warn-not-force makes webhooks/retention **fail-open** under paranoid (explicit
operator config beats the preset). This is the intended informed-consent
posture; absent explicit config, paranoid still yields no egress (the default is
none). Reversible later if we want a fail-closed "strict" tier.

## Verification

- `gofmt -l` clean on touched files; `go build ./...`; `go test
  ./internal/config/... ./cmd/coordinator/...`.
- Manual: load `operator.paranoid.yaml` (webhooks + retention 30) → webhooks
  retained, 2 warnings; load `operator.minimal.yaml` (paranoid off) → no
  warnings, recording on.
