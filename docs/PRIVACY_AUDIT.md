# Privacy Audit

Status: **Phase 0 — normative** for the privacy posture of the
production stack. This document tracks the telemetry / phone-home /
fingerprinting behaviour of every dependency Nova ships with, the
hardening defaults applied, and the behaviour of the operator's
`paranoid: true` switch.

## Purpose

A privacy-paranoid operator should be able to answer four questions
from this document alone:

1. Does any component in the deployment connect to a third party
   under default configuration?
2. What does Nova change from those defaults?
3. What additional traffic does `paranoid: true` eliminate?
4. What residual risks remain after `paranoid: true`?

If the answers ever drift from what is documented here, that is a
bug. Phase 1+ adds CI lint and integration tests that assert the
hardened defaults are not silently regressed.

## Dependency-by-dependency posture

Every external dependency in the production deployment, with its
default phone-home behaviour and Nova's hardening posture.

| Dependency | Default phone-home? | Hardening Nova applies | Notes |
|---|---|---|---|
| **Go runtime** | None | n/a | No telemetry. |
| **Postgres 16** | None | n/a | No telemetry. `pg_stat_statements` is local-only. |
| **Kubo** | **Yes — significant** | Forced empty `Bootstrap`, `Routing.Type=none`, `Discovery.MDNS.Enabled=false`, `Provider.Strategy=""`, `Reprovider.Strategy=""`, loopback-only API/Gateway. Refuses to start without `IPFS_SWARM_KEY` in private mode. See `docs/specs/KUBO_HARDENING.md`. | Default Kubo joins the public DHT, broadcasts every CID it provides, listens on mDNS, and announces to public bootstrap peers. **All of this is shut off by default.** |
| **IPFS Cluster** | None at app layer | Cluster API on internal interface only | Cluster's pin coordination is intra-federation. |
| **Nebula** | None | n/a | No telemetry, no phone-home. Lighthouses route handshakes only and do not see traffic. |
| **nginx (open source)** | None | Access logs default-truncated to 30 days; ≤ 24 hours in paranoid mode | Mainline nginx OSS has no telemetry. (nginx Plus is not used.) |
| **certbot** | None at runtime | Operator chooses TLS mode (HTTP-01 / DNS-01 / static / `.onion`). Default is to **prompt** the operator on first run with the CT-log disclosure. | The certificate issuance itself, with HTTP-01 or DNS-01, leaves a CT-log entry. See "TLS mode" below. |
| **Authelia** | None by default | Operator-supplied OIDC; runs locally. | Optional dependency; only present if operator enabled SSO. |
| **chi (Go HTTP router)** | None | n/a | Pure Go library. |
| **pgx (Postgres driver)** | None | n/a | Pure Go library. |
| **sqlc** | None | n/a | Build-time codegen; not present at runtime. |
| **govips → libvips** | None | n/a | C library; no network calls. |
| **goimagehash** | None | n/a | Pure Go; no network calls. |
| **Uppy** (`@uppy/core` + `@uppy/tus`) | None for the core/tus modules | Companion (the third-party-cloud upload module) is not bundled | Uppy ships ad-hoc analytics in some optional plugins; we exclude those. |
| **tus.io protocol** | None | n/a | Pure protocol; no telemetry. |
| **React + Vite** (admin SPA) | None at runtime | Hermetic build: CI lint blocks references to `fonts.googleapis.com`, `cdnjs.cloudflare.com`, `unpkg.com`, etc., in the built bundle. Strict CSP `default-src 'self'` on every served page. | Vite has no production telemetry. |
| **Prometheus client (Go)** | None | n/a | Metrics endpoint is operator-controlled. |
| **OpenTelemetry-Go** | None at default | Off in `paranoid: true` mode | Optional. Operator chooses an export destination if used. |
| **Toxiproxy / Pumba** | None | n/a | Test-only; never present in production binaries. |
| **Docker / docker-compose** | None | n/a | Docker Engine on Linux has no telemetry. |
| **Docker Desktop** (macOS / Windows) | **Yes** | Documented in `VOLUNTEER_CHECKLIST.md`; recommend Linux + Docker Engine for donor nodes | Not software-enforceable. |

## Hardening defaults applied at startup

The coordinator and donor binaries refuse to start if any of these
are violated:

- `IPFS_SWARM_KEY` env var is unset and `public_ipfs_dht: false`
  (the default).
- Kubo `Discovery.MDNS.Enabled = true`.
- Kubo `Bootstrap` contains a public-internet entry (validator
  rejects anything outside loopback / RFC 1918 / operator overlay).
- Kubo `Provider.Strategy` or `Reprovider.Strategy` is non-empty.
- Kubo `Routing.Type` is anything other than `none` or `dhtclient`
  (in private mode).
- Kubo API or Gateway listens on a non-loopback interface.
- The container running as root.
- `auth: anonymous` and `moderation: off` set simultaneously.
- Public uploads enabled without `tos_url` set in operator config.

The validator emits precise error messages naming the offending key.
"Refusal to start" is preferable to "start with a warning" for
hardening rules; warnings get ignored in production runbooks.

## TLS mode and CT log exposure

Let's Encrypt logs every issued certificate's hostname to public
Certificate Transparency logs **forever**. For an operator who
intends their hostname to remain private, default ACME HTTP-01 is
not appropriate.

The first-run wizard prompts:

| Mode | Description | Privacy posture |
|---|---|---|
| `quick-setup` | Runs certbot HTTP-01 against Let's Encrypt | Hostname is public in CT logs. Convenient. |
| `dns-01-wildcard` | Operator obtains `*.example.com` cert via DNS challenge | Hostname not visible in CT logs; needs DNS API access at the operator's nameserver |
| `static` | Operator supplies cert + key files | Operator controls everything |
| `.onion` | Self-signed cert behind a Tor hidden service | No public DNS, no CT |

`quick-setup` displays a mandatory disclosure screen describing the
CT-log implication before completing.

## `paranoid: true` mode

`paranoid` is **default-off** and works as a *preset*, not a hard override
(P2-M0.2). When enabled it sets protective **defaults** for the settings below;
any value the operator set explicitly **wins**, and relaxing a protective
default emits a consequence **warning** (coordinator startup log; surfaced
inline in the admin Settings UI) rather than being silently overridden or
refused. An operator can adopt the hardened posture and still make a
deliberate, logged exception.

Two caveats: (1) defaulting off means none of these protections are active
unless the operator opts in; (2) legal/safety floors are **not** part of this
preset and still **refuse to start** when violated — see "Warn, don't force"
below.

| Setting | Default (paranoid off) | `paranoid: true` preset |
|---|---|---|
| Source-IP recording (`coordinator.record_source_ip`) | record (true) | off; **explicit `true` kept with a warning** |
| Source-IP retention (`source_ip_retention_days`) | 30 days | 1 day; **higher explicit value kept with a warning** |
| Outbound webhooks (`webhooks`) | honoured | none added; **explicit destinations kept with a warning** (egress stays active) |
| Public IPFS DHT (`coordinator.public_ipfs_dht`) | private (false) | private; **enabling warned** |
| ACME automation | operator's TLS mode | off — supply cert + key files (CT-log exposure) |
| nginx access log retention | 30 days | ≤ 24 hours |
| OpenTelemetry export | off by default; honoured if configured | off |
| Prometheus metrics endpoint | loopback-bound | loopback-bound; widening refused (defense-in-depth) |
| Kubo hardening validator | strict (private) / relaxed (public-DHT) | strict |
| Update checks / supporters page | none / opt-in | none / disabled |

The first four rows are the operator-tunable privacy settings the preset
governs with warn-not-force semantics. The remainder are enforced or
informational defaults; metrics-exposure widening stays refused for now
(defense-in-depth), to be folded into the tunable model in a later milestone.

### First-run wizard surface (P2-M0.5)

The first-run setup wizard now exposes the three operator-tunable constituents
individually — source-IP recording, retention period, and public-DHT exposure —
with inline consequence copy at the moment the operator makes the decision (see
`docs/superpowers/specs/phase2/2026-06-14-m0.5-setup-wizard-redesign-design.md`). A
fully-hardened wizard run (all three checked) writes the explicit constituent
values to `operator.yaml` and produces **no** `ApplyPrivacyPreset` startup
warnings; warnings remain the signal for hand-edited drift from a protective
default.

### Admin Settings screen surface (P2-M0.6)

The admin console's operator-only **Settings** screen now surfaces the same three
constituents at *runtime* with the same consequence copy (see
`docs/superpowers/specs/phase2/2026-06-15-m0.6-settings-screen-design.md`), driving the
M0.4 config API. Because runtime nodes — unlike first run — can have outbound
webhooks configured, the screen derives `auth.paranoid` as the AND of the three
hardened children **and** an empty `webhooks` list, and renders the parent
indeterminate when webhooks exist; so a save can never trip an `ApplyPrivacyPreset`
warning. Saving resolves drift from the editable constituents; a
webhook-induced warning persists until `webhooks` is cleared via `novactl config`
or a hand-edit.

### Warn, don't force — and the hard floors

The preset never *prevents the node from operating* because a privacy default
was relaxed; it warns. The floors that **do** refuse to start are legal/abuse
gates, not privacy preferences, and are unchanged:

- `uploads.public_uploads: true` with no `tos_url` (T1.20).
- `auth.anonymous: true` with no moderation flow.

The mode is intended for operators who must demonstrate to their community that
the deployment cannot phone home, and for deployments in adversarial
environments. It does not weaken Nova's encryption or replication; it only
constrains side channels.

## Specific telemetry concerns and their resolutions

### Kubo's default DHT participation

**Concern.** A default Kubo install advertises every CID it pins to
the public DHT, broadcasts on the LAN via mDNS, and connects to
public bootstrap peers. A donor running default Kubo would leak the
federation's CID set and identify themselves as a Nova donor to
anyone scraping the DHT.

**Resolution.** The donor's Kubo configuration is hardened at boot
and the daemon refuses to start if hardening is violated. See
`docs/specs/KUBO_HARDENING.md` for the full validator rules.

### Let's Encrypt CT-log exposure

**Concern.** HTTP-01 certificate issuance creates a public,
permanent record of the operator's hostname.

**Resolution.** TLS mode is selectable; the first-run wizard
displays the trade-off explicitly. Operators in privacy-sensitive
contexts should choose DNS-01 wildcard, static certs, or `.onion`.

### Admin SPA fingerprinting

**Concern.** Many web frameworks fetch fonts, icons, or libraries
from third-party CDNs at runtime, fingerprinting administrators to
those CDNs every page load.

**Resolution.** Hermetic build: every asset is compiled into the
served bundle. CI lint (`hermetic-spa.yml`, Phase 1) greps the
built bundle for any reference to known third-party origins and
fails the build on a hit. CSP `default-src 'self'` blocks runtime
fetches from anywhere else.

### Grafana

**Concern.** Grafana switched to AGPL with v8 and ships with
telemetry on by default. AGPL is a license-compatibility problem;
default-on telemetry is a privacy problem.

**Resolution.** Grafana is **not** part of the recommended Nova
stack. The admin SPA renders metrics directly from the Prometheus
HTTP API. Operators who want Grafana opt in manually and accept the
trade-off.

### Docker Desktop on macOS / Windows

**Concern.** Docker Desktop sends usage telemetry by default. Donor
operators on macOS or Windows running Docker Desktop would phone
home about the federation's existence, even if Nova itself does not.

**Resolution.** `VOLUNTEER_CHECKLIST.md` recommends Linux + Docker
Engine for donor nodes. Not software-enforceable; operators
following the checklist arrive at a clean configuration.

### Adapter modules

**Concern.** Nova's adapter modules (`nova-mastodon`,
`nova-discourse`, etc.) live in separate repositories with their
own dependency surfaces. An adapter could ship telemetry that
operators inadvertently inherit.

**Resolution.** Adapters import only Nova's public HTTP API. They
do not link the coordinator's binaries; they cannot exfiltrate
master keys, blob plaintexts, or audit logs because they do not
have access to them. Operators audit adapters individually before
installing.

## Residual risks after `paranoid: true`

These are honest about what `paranoid: true` does **not** mitigate:

1. **Operator host-level telemetry.** If the operator runs the
   coordinator on a Docker Desktop install, a Mac with Apple
   analytics, a managed VPS that ships kernel telemetry, etc.,
   Nova cannot stop those signals. The host environment is the
   operator's problem.
2. **Network-volume analysis.** Even with all phone-home disabled,
   an external observer can fingerprint Nova's traffic patterns
   over time.
3. **Operator user-error.** A misconfigured nginx that proxies the
   coordinator to a public CDN with logging enabled would leak
   request paths regardless of what the coordinator does. The
   reference nginx configuration ships with safe defaults but
   operators can override them.
4. **DNS query telemetry.** Even with hostname allowlisting,
   queries to allowlisted hostnames still flow through the
   operator's recursive resolver. Operators in extreme threat
   environments should run their own resolver.
5. **TLS handshake metadata.** SNI, certificate fingerprint, and
   cipher-suite preferences are observable. Encrypted Client Hello
   (ECH) deployment is a Phase 6+ research direction.

## How this audit is maintained

This document is updated whenever:

- A dependency is added, removed, or upgraded across a major version.
- The Kubo hardening rules change.
- A new `paranoid: true` behaviour is added.
- A privacy-relevant CVE is published against a dependency Nova
  uses.

PRs that change the stack without updating this document are
rejected. The CONTRIBUTING.md PR checklist will reference this
explicitly in Phase 1.
