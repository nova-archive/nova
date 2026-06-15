# P2-M1 — Build / repo separation (`nova-node`)

Status: **design**. Spec floor: the P2-M0-amended normative specs in `docs/specs/`
(FED/HEAL/POSS/ENVELOPE v-bumped, `DATA_MODEL`/`ARCHITECTURE_DECISIONS` updated) and
the Phase-2 master design. Implementation plan generated under writing-plans:
[`../../plans/phase2/2026-06-15-phase2-m1-build-repo-separation.md`](../../plans/phase2/2026-06-15-phase2-m1-build-repo-separation.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

## Context

Phase 1 shipped a single-host Nova (`v0.1.0-rc1`): one coordinator, embedded
hardened Kubo, everything on one machine. Phase 2 makes data durable across
volunteer-run **donor nodes** over a private Nebula mesh, and (independently) lifts
the envelope to streaming-AEAD. The master design
([`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md))
splits the phase into eleven milestones. **P2-M0 (spec reconciliation) is merged**
(tag `p2-m0-spec-reconciliation`); the P2-M0.x operator-UX/privacy remediation track
is complete. Additive Phase 2 is unblocked.

**P2-M1 is the first code milestone.** Its single load-bearing goal is *not* "a
donor that federates" — it is **a donor artifact (`nova-node`) whose dependency
graph provably excludes operator-only code**, built and signed as a separate
minimal image, that loads + validates its config and runs a health endpoint. There
is **no live federation**: registration, transport, assignment sync, healing,
possession audits, and the v2 envelope are all later milestones (M2–M10). M1 builds
the *permanent boundaries and the release-pipeline shape* and nothing more.

The master plan (`../../plans/phase2/2026-06-11-phase2-federation.md` § P2-M1)
already sketches the task list. This document is the dedicated design that expands
it and ratifies the two decisions the master plan left implicit (config-package
placement; supply-chain depth).

## Decisions (ratified with Bug, 2026-06-15)

- **Config: shared leaf + donor-local schema.** The generic, stdlib-only
  secret-precedence resolver moves out of `internal/config` into a new
  `internal/secret` leaf package imported by *both* binaries. The donor `node.yaml`
  schema + validation live in donor-local `internal/node/config` — **never**
  `internal/config`, whose conceptual centre is the operator `operator.yaml` (plus
  the privacy preset, the live-reload store, and config-admin metadata). `cmd/node`
  must not import `internal/config`; the boundary allowlist excludes it. Rationale:
  M1's deliverable is a *provably* separated artifact, so the package boundary
  should make the safe thing mechanically obvious rather than relying on the CI gate
  to catch a future re-pollution of a shared operator package. The resolver is
  genuinely generic (env → `_FILE` → mount), so it is *extracted* (one home), not
  *duplicated*.
- **Supply chain: stand up the real CI pipeline now; volunteer docs at M7.** M1
  authors the blocking `donor-deps-boundary` job, a `donor-build` job, and a
  `donor-sbom-sign` job (syft SBOM + cosign **keyless/OIDC** signing + GitHub
  provenance attestation). Locally verifiable today: the boundary script, the image
  build, and SBOM generation. Keyless signing is authored and exercised when CI runs
  on push. **No local cosign key-pair signing path** — a second signing model would
  invite "is the local signature trustworthy?" confusion. The single release-trust
  path is keyless/OIDC. The volunteer-facing digest-pin walkthrough, `cosign verify`
  drills, and revocation/provider-loss runbooks land at **P2-M7** (which already
  owns "signed images + SBOM + provenance" for both images).
- **Health binds loopback in M1.** With no Nebula interface yet, a bare `nova-node`
  run binds its health endpoint to a configured `health_listen_addr` defaulting to
  loopback. The federation listener that binds the Nebula interface only is an M2+
  concern; M1 documents it but does not build it.
- **`deploy/donor/` is a new top-level tree.** Per the master design's authoritative
  layout (`deploy/donor/`, `deploy/operator/`). The coordinator compose stays at
  `docker/docker-compose.yml` for now; relocating it under `deploy/operator/` is a
  later, separate move, out of M1 scope.
- **No migration.** M1 touches no schema. The Phase-2 schema deltas (D6–D10 +
  `blob_replication_state`) ship as a new migration in **P2-M3**; the
  `migrations-frozen` gate stays green.
- **`bandwidth` gets real arithmetic; the rest are stubs.** The donor token-bucket
  is the authoritative budget enforcer (D11) and is pure, dependency-free, and
  testable, so M1 implements its real refill/take/refuse-over-budget logic. `agent`,
  `state`, `transfer`, and `audit` are interface + compile-time stub only — their
  real behavior is M2/M4/M6.

## Scope

**In scope.** The donor binary skeleton (`cmd/node`); donor-local config
(`internal/node/config`); the shared `internal/secret` leaf (extracted) and
`internal/federation/wire` types (incl. the Ed25519 repair-token claim + `Verify`,
no mint); the `internal/node/{agent,state,bandwidth,transfer,audit}` skeletons; the
`go list -deps`-based dependency-boundary gate; split Dockerfiles + a minimal
`node.Dockerfile`; `deploy/donor/`; the CI SBOM + keyless-signing pipeline; Makefile
targets.

**Out of scope (later milestones).** Live mTLS-over-Nebula transport, registration,
capability negotiation handshake wiring, cert rotation/revocation (M2); the
`pin_changes` log + assignment sync + snapshot recovery (M3); coordinator-as-source,
streaming transfer + deterministic re-import + root-CID verify, Ed25519 token
*minting*, donor↔donor repair, embedded Kubo on the donor (M4); liveness + healing +
`blob_replication_state` (M5); possession audits + reputation (M6); volunteer
release docs + digest-pin/verify drills (M7); the v2 streaming envelope (M8–M10). No
DB, no migration, no operator-side behavior change beyond the mechanical
`internal/secret` re-point.

## The dependency boundary (the load-bearing contract)

The milestone exists to make this true and keep it true:

> The `cmd/node` build graph contains **no operator-only code**.

Mechanically enforced by `scripts/check_node_deps.sh`, run as a **blocking** CI job
(`donor-deps-boundary`): it computes `go list -deps ./cmd/node` and fails if the
graph contains any disallowed root. We use an **allowlist** of expected donor-safe
roots (deny-by-default) rather than a denylist, so a newly added operator package is
rejected by default instead of silently slipping through.

- **Disallowed (operator-only):** `internal/masterkey`, `internal/moderation`,
  `internal/auth`, `internal/setup`, `internal/db`, **`internal/config`**,
  `internal/api/handlers/admin*`, `nova-image`, `pkg/coordinator`.
- **Allowed (donor-safe leaves):** `internal/secret`, `internal/node/*`,
  `internal/federation/wire`, plus stdlib and a minimal vetted third-party set
  (`gopkg.in/yaml.v3` for config; `crypto/ed25519` is stdlib). The M1 health
  endpoint uses stdlib `net/http` — **not** chi — to keep the donor graph minimal.
  Any future addition to the allowlist is a deliberate, reviewed edit.

`internal/config` is on the *disallowed* side specifically because it is the
operator config package and will keep growing operator-facing surface (it already
owns the privacy preset and the live-reload store). The `internal/secret` extraction
is what lets the donor read secrets without importing it. The boundary is the point
of the milestone — the CI job is **blocking, not advisory**, and review includes a
deliberately-injected violation (a throwaway `internal/db` import in `cmd/node`,
reverted before commit) to prove the gate actually fails red.

## Package & component layout

```
cmd/
  node/main.go            donor entrypoint: load+validate node.yaml, health, (stub) agent

internal/
  secret/                 ResolveSecret + SecretSource (+Source*) — stdlib leaf, shared (EXTRACTED from internal/config)
  federation/wire/        shared protocol + Ed25519 token claim + Verify + capability ids + error codes (NEW)
  node/                   donor-only (NEW)
    config/               node.yaml schema + refuse-to-start validation
    agent/                register→heartbeat→sync loop skeleton (no-op transport in M1)
    state/                local cursor/cert/replay store interface (file/KV; NO Postgres) + stub
    bandwidth/            authoritative token-bucket — REAL arithmetic
    transfer/             streaming fetch + re-import + root-CID verify interface (stub)
    audit/                possession-challenge responder interface (stub)
  config/                 operator-only; loses secrets.go (re-points to internal/secret)
```

- **`internal/secret`** — the only operator/donor *shared* package besides
  `federation/wire`. Pure `os`/`io`/`strings` leaf; carries `ResolveSecret`,
  `SecretSource`, and `Source{Env,FileEnv,Mount}` verbatim from
  `internal/config/secrets.go`. The two existing callers
  (`cmd/coordinator/main.go`, `internal/envelope/keystore.go`) re-point; the test
  moves with it. Behavior is unchanged — the coordinator suite is the guard.
- **`internal/federation/wire`** — pure types: register/heartbeat/`pins/changes`/
  snapshot/ack/fail request+response structs; capability identifiers (`fed/v1`,
  `pin-change-log/v1`, `snapshot/v1`, `repair-stream/v1`, `audit-block-hash/v1`); a
  capability-negotiation helper (intersect supported sets → select, or fail closed);
  normalized error codes including the machine-readable `snapshot_required` (D7); and
  the **Ed25519 repair-token claim** struct (`jti`, `assignment_id`, `generation`,
  `cid`, `source_node_id`, `dest_node_id`, `not_before`, `not_after`, `max_bytes`,
  `protocol_version`) plus `Verify(pub, token)` (signature + time-window + structural
  validity, stdlib `crypto/ed25519`). **Minting is coordinator-side and lands in
  M4**; single-use replay enforcement is source-side state and is wired in M4 — M1
  ships the verifiable type so both binaries agree on the wire shape now.
- **`internal/node/*`** — see Decisions. `bandwidth` is real; the rest are
  interfaces with stub implementations and compile-time conformance assertions.

## Runtime behavior in M1

`nova-node` accepts `--config <path>` and `--validate`.

- `nova-node --validate --config node.yaml` loads + validates and exits: `0` on a
  valid config, non-zero with a clear diagnostic on a malformed/incomplete one. This
  is the milestone's headline behavior and the donor analogue of the coordinator's
  startup floor.
- `nova-node --config node.yaml` (bare run) loads + validates, binds a health
  endpoint to `health_listen_addr` (default loopback), starts the **no-op** agent
  loop, and blocks until a termination signal — mirroring `cmd/coordinator`'s
  signal-driven lifecycle. No outbound connections, no Nebula, no Kubo.

## Donor config (`node.yaml`)

`internal/node/config` defines and validates the donor configuration; secrets
resolve through `internal/secret.ResolveSecret`. Fields:

- `coordinator_url` — the coordinator federation endpoint (used from M2).
- `federation_cert` / `federation_key`, `nebula_cert` / `nebula_key` — paths; the
  two-cert model (Nebula authorizes the overlay, federation authorizes the HTTP API).
- `swarm_key` — the private-IPFS swarm key path.
- `storage_dir` — where the donor will hold replica ciphertext.
- `bandwidth_budget_bytes_per_day` — the authoritative budget feeding the
  token-bucket.
- `failure_domain` — operator-declared hints (`provider`, `asn`, `region`); these are
  *self-declared* and become authoritative only once operator-verified at the
  coordinator (D8) — informational at the donor.
- `health_listen_addr` — default loopback (see Runtime behavior).

Validation **refuses to start** on: missing cert/key paths, missing swarm key,
missing/empty `storage_dir`, non-positive `bandwidth_budget_bytes_per_day`, or
malformed YAML. Table-driven tests cover the minimal-valid case and each
missing-required field.

## Artifact & deployment

- **Split Dockerfiles.** Rename `docker/Dockerfile` → `docker/coordinator.Dockerfile`
  (contents unchanged) so the two images build independently; update
  `make docker-build`, `scripts/smoke.sh`, and the compose `build:` context to the
  new path. Add `docker/node.Dockerfile`: multi-stage, **CGO-off pure-Go** (no
  libvips), runtime layer containing only `nova-node` + CA certs + a minimal health
  tool. Non-root; read-only rootfs; `cap_drop: [ALL]`; `no-new-privileges`; a single
  writable data volume. The image contains **none** of: libvips, Node/web bundles,
  `migrate`, `novactl`, Postgres client, master-key code, or operator secret paths.
- **`deploy/donor/`.** `compose.yaml` brings up a Nebula sidecar (host/shared-netns
  process — `nova-node` needs no `NET_ADMIN`) plus `nova-node`, with **no published
  ports** and the M14 hardening floors (healthcheck, read-only rootfs + tmpfs,
  `no-new-privileges`, `cap_drop: [ALL]`), and volumes for donor state + ciphertext.
  `node.yaml.example` is the annotated donor config. The donor is deliberately **not**
  a profile in the operator compose — different actor, trust, secrets, and ports.

## Supply chain (D-sign promotion)

Three CI jobs in `.github/workflows/ci.yml`:

- `donor-deps-boundary` — runs `scripts/check_node_deps.sh`; **blocking**.
- `donor-build` — builds the donor image from `docker/node.Dockerfile`.
- `donor-sbom-sign` — generates a per-image SBOM with **syft**, signs the image with
  **cosign keyless/OIDC**, and attaches a GitHub provenance attestation.

What is verifiable in the local (no-push) workflow: the boundary script, the image
build, and `make node-sbom` (syft). Keyless signing is authored and is exercised the
first time CI runs on a pushed branch. The single release-trust path is keyless/OIDC;
M1 adds **no** local key-pair signing. A high-level release-trust note goes in the
donor docs stub; the full volunteer-facing digest-pin + `cosign verify` walkthrough
is **P2-M7**.

## Forward-compatibility (post-1.0 HA, named not built)

The `internal/federation/wire` token claim already carries `generation` and
`assignment_id` — the immutable handles a Phase-6 multi-endpoint donor would fail
over on, per the master design's governing rule (build structures Phase 2 needs that
*also* serve Phase 6/7; never build Phase-6-only runtime logic). M1 builds no
fencing tokens, no control-plane leases, no multi-endpoint failover — it only fixes
the wire shape so those land additively later.

## Testing strategy

- **Unit.** `internal/secret` resolver table (moved, must stay green);
  `internal/federation/wire` token `Verify` table (valid / expired / not-yet-valid /
  wrong-key / tampered-claim / replayed-`jti` shape) and capability negotiation
  (overlap → select; disjoint → fail-closed); `internal/node/config` validation
  table; `internal/node/bandwidth` token-bucket arithmetic (refill, take,
  refuse-over-budget); `cmd/node` `--validate` exit codes (good/bad config), mirroring
  `cmd/coordinator/main_test.go` and `cmd/novactl/main_test.go`.
- **Boundary.** `donor-deps-boundary` green on the branch; demonstrated red against a
  reverted injected operator-only import.
- **Build/CI.** `donor-build` produces an image; `donor-sbom-sign` emits an SBOM +
  signature on push; `go build ./... && go vet ./...` stay green; the coordinator
  `scripts/smoke.sh` is unaffected by the Dockerfile rename.

## Exit criteria

1. `go build ./cmd/node` succeeds; `go build ./... && go vet ./...` green.
2. `scripts/check_node_deps.sh` exits 0; it exits non-zero against an injected
   operator-only import (demonstrated in review).
3. `nova-node --validate` accepts a valid `node.yaml` and rejects a malformed one
   with a clear diagnostic.
4. `make node-image` builds a minimal donor image whose inventory excludes
   libvips/Node/`novactl`/`migrate`/master-key.
5. CI produces a donor SBOM and a keyless signature on push.
6. The coordinator build, tests, and smoke are unchanged by the `internal/secret`
   extraction and the Dockerfile split.
7. **No live federation**, no schema/migration, no operator behavior change.

## Gotchas & deferrals

- **Nebula is a sidecar, not a Go dependency.** Do not pull a Nebula library into
  `cmd/node` — it would bloat the artifact and widen the boundary. The donor binds
  the Nebula interface address discovered from config (M2+).
- **Frozen migrations.** M1 adds none; never edit `internal/db/migrations/000*`. The
  Phase-2 schema lands in M3.
- **The boundary CI must be blocking.** Any accidental `internal/config`/`internal/db`
  import from a shared helper re-pollutes the donor graph; advisory-only would defeat
  the milestone.
- **The `internal/secret` move is behavior-preserving.** It must not change how the
  coordinator resolves or logs secret sources; the existing coordinator tests are the
  regression guard.
- **Embedded Kubo / `go-car`/`internal/ipfs` reuse on the donor is M4**, evaluated
  for donor-safety then (and added to the allowlist deliberately if it qualifies).

## Cross-references

- Master design + plan:
  [`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md),
  [`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).
- Amended normative specs (P2-M0): `docs/specs/FEDERATION_PROTOCOL.md`,
  `ENCRYPTION_ENVELOPE.md`, `HEALING_PROTOCOL.md`, `POSSESSION_AUDIT.md`,
  `DATA_MODEL.sql`, `ARCHITECTURE_DECISIONS.md`.
- Threat model: `docs/THREAT_MODEL.md` § Phase-2 amendment (D-sign donor signing).
- Donor operations: `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md` (digest-pin + `cosign
  verify` mature at P2-M7).
- This milestone's plan:
  [`../../plans/phase2/2026-06-15-phase2-m1-build-repo-separation.md`](../../plans/phase2/2026-06-15-phase2-m1-build-repo-separation.md).
