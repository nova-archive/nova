# P2-M2 — Identity, registration, capability negotiation

Status: **design**. Spec floor: the P2-M0-amended normative specs in `docs/specs/`
(FED/HEAL/POSS/ENVELOPE v-bumped, `DATA_MODEL`/`ARCHITECTURE_DECISIONS` updated),
the Phase-2 master design
([`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md)),
and the P2-M1 build/repo-separation design
([`2026-06-15-phase2-m1-build-repo-separation-design.md`](2026-06-15-phase2-m1-build-repo-separation-design.md)).
Implementation plan generated under writing-plans:
[`../../plans/phase2/2026-06-16-phase2-m2-identity-registration.md`](../../plans/phase2/2026-06-16-phase2-m2-identity-registration.md).

Authors: Bug Plowman (operator), Claude (implementation partner).

## Context

Phase 1 shipped a single-host Nova (`v0.1.0-rc1`). Phase 2 makes data durable
across volunteer-run **donor nodes** over a private Nebula mesh. **P2-M1 is
merged** (tag `p2-m1-build-repo-separation`): a `nova-node` donor binary whose
dependency graph provably excludes operator-only code, shared
`internal/federation/wire` types (register/heartbeat/changes/ack/fail messages,
capability negotiation, the Ed25519 repair-token claim + `Verify`), donor-only
`internal/node/{config,agent,state,bandwidth,transfer,audit}` seams (only
`bandwidth` has real logic), split Dockerfiles, the deny-by-default
`donor-deps-boundary` CI gate, and the keyless SBOM/sign pipeline. **There is no
live federation** in M1: the agent loop is a no-op, the state store is an
in-memory stub, and the donor opens no outbound connections.

**P2-M2 lights up the authenticated control channel.** It is the first milestone
where a donor and the coordinator actually talk: mTLS-over-Nebula identity, the
`POST /fed/v1/register` and `POST /fed/v1/heartbeat` endpoints, node identity
derived from the **verified federation certificate**, capability negotiation
(fail-closed), `trust_state` assigned at registration, and the operator's
federation-CA / cert-issuance / revocation tooling (`novactl node …`). It builds
the *registry and the trust fabric* and nothing about pins — assignment sync,
transfers, repair tokens, healing, and possession audits are all later
milestones (M3–M6).

The master design's milestone line for M2:

> **P2-M2** — Identity, registration, capability negotiation. Nebula sidecar
> path; mTLS federation identity; stable `node_id` from verified cert;
> register/heartbeat; cert rotation/revocation; protocol + capability selection
> (fail-closed); `trust_state` assigned.

This document expands that line and ratifies the decisions the master plan left
implicit (schema boundary, CA model, listener wiring, cert-identity mechanics).

## Decisions (ratified with Bug, 2026-06-16)

- **M2 ships a `nodes`-scoped first Phase-2 migration.** M2 is the first live
  federation milestone, so node identity, registration, negotiated
  protocol/capability state, cert revocation/rotation bookkeeping, and the
  initial trust assignment must be **durable coordinator state** — not an
  in-memory simulation. M2 therefore ships a new forward-only migration scoped to
  the **`nodes` table + node-registration queries only**. This **amends the
  earlier P2-M0/M1 planning note that all Phase-2 executable DDL lands in P2-M3.**
  The governing principle: *schema lands when a milestone first needs durable
  truth, not when a later milestone first consumes the broader subsystem.* M3
  remains the owner of `pin_changes` / `pin_assignments.assignment_id` /
  `generation`; M5 owns `blob_replication_state` and the D8 failure-domain /
  `placement_weight` columns; M6 owns the possession-audit schema and trust
  *graduation*. No shipped migration is edited; `migrations-frozen` stays green.

- **The federation X.509 layer is Nova's; Nebula stays an external sidecar.**
  `novactl node` owns a **Nova federation X.509 CA** (pure-Go `crypto/x509`, no
  external binary) that signs donor federation client certs and the coordinator's
  federation server cert — the certs the mTLS layer actually verifies and that
  identity derives from. **Nebula remains a host/sidecar runtime** provisioned
  with the upstream `nebula-cert` tool; Nova does **not** import a Nebula library
  and does **not** shell out to `nebula-cert` in core M2 flows. `novactl node`
  emits Nebula config templates, file-layout conventions, lighthouse/interface
  placeholders, and operator docs so the overlay is *guided but not owned*. This
  preserves the M1 donor dependency boundary, keeps M2 tests hermetic over
  loopback TLS, and anchors identity to the verified federation mTLS cert rather
  than self-asserted JSON or overlay metadata.

- **Federation server is standalone, not folded into `pkg/coordinator`.** A
  dedicated `internal/federation/coordinator` server, constructed and run by
  `cmd/coordinator` as a **second `http.Server` on its own listener**. The
  public, semver-stable `pkg/coordinator` surface stays unaware of federation.

- **Repair-token signing key is deferred to M4.** M2 has no transfers, no donor
  inbound repair endpoint, no coordinator-as-source, and no token minting. The
  heartbeat **delivery channel** is built (`repair_token_public_key` field
  present) but carries an **empty** value until M4 stands up the Ed25519 signer.

- **Donor durable state is an atomic JSON file.** No bbolt, no embedded KV. M2's
  small state (`node_id`, federation cert fingerprint, registration
  confirmation, selected protocol) lives in
  `storage_dir/state/registration.json`, written atomically (temp → fsync →
  rename → dir-fsync), `0600`. Cursor / jti-replay durability stays deferred to
  M3 / M4.

## Scope

**In scope.** The shared mTLS transport (`internal/federation/transport`); the
coordinator federation server + listener (`internal/federation/coordinator`) with
`register` + `heartbeat` handlers; DER-leaf cert-fingerprint identity with the
`node_id` UUID bound in the cert's URI SAN; capability negotiation persistence;
`trust_state='probationary'` at register; the donor register→heartbeat agent loop
(`internal/node/agent`) over the mTLS client; atomic JSON registration state
(`internal/node/state`); the `nodes`-scoped migration `0011` + `queries/federation.sql`;
`novactl node ca-init/issue/revoke/rotate-cert/list/nebula-template`; the operator
`federation` config block; operator + donor Nebula-sidecar compose wiring (minimal).

**Out of scope (later milestones).** `pin_changes` log + snapshot/epoch recovery +
node-local cursor (M3); coordinator-as-source, streaming transfer, Ed25519
repair-token mint + donor↔donor repair endpoint, embedded Kubo on the donor (M4);
the 5-state liveness **sweeper** (timer-driven `suspect`/`unreachable`/`evicted`
transitions), healing, `blob_replication_state`, D8 failure-domain anti-affinity +
`placement_weight` (M5); possession audits, reputation, trust **graduation** (M6);
volunteer release runbooks + digest-pin/`cosign verify` drills (M7); the v2
streaming envelope (M8–M10). **Zero-downtime cert rotation overlap** is out unless
explicitly added — M2 ships downtime cutover (see § Cert rotation).

## Architecture

### Two listeners, one process

`cmd/coordinator` runs the existing public/admin coordinator **and** a new
federation server concurrently, both honoring the same signal context. The wiring
uses an errgroup-style run loop: **both listeners bind before startup is declared
successful**, and failure of either server cancels the shared context and drains
the other — never a partial state where the public coordinator is live but the
federation listener failed to bind.

```
cmd/coordinator
  ├─ pkg/coordinator.Coordinator.Run(ctx)      public/admin mux (unchanged)
  └─ internal/federation/coordinator.Server.Run(ctx)
        mTLS listener on federation.listen_addr (the coordinator's Nebula overlay addr)
        /fed/v1/register, /fed/v1/heartbeat
```

`internal/federation/coordinator.Server` is constructed with a `*gen.Queries`
(its only operator-state dependency), the federation TLS material, and the
resolved `federation` config. It serves a stdlib `net/http` mux — it reuses no
operator API middleware (the donor-deps-boundary forbids `internal/api` on the
donor side; the coordinator side simply has no need for it here).

### `internal/federation/transport` (shared mTLS)

The one new package both binaries import (alongside `wire` and `secret`). Pure
stdlib `crypto/tls` + `crypto/x509`:

- **Server config:** `tls.Config{ClientAuth: RequireAndVerifyClientCert,
  ClientCAs: <federation CA pool>}` plus the coordinator's federation server
  cert. A `VerifyConnection` / handler-side hook extracts the verified leaf.
- **Client config:** the donor's federation client cert + key and the federation
  CA pool as `RootCAs`, so the donor verifies the coordinator's server cert too
  (mutual).
- **Identity extraction:** from the verified leaf, compute
  `federation_cert_fingerprint = "sha256:" + hex(sha256(leaf.Raw))` (**DER of the
  leaf certificate**, not SPKI — revocation/rotation track the certificate, not
  the key) and parse the `node_id` UUID out of the leaf's **URI SAN**
  (`nova://node/<uuid>`).

The overlay is transparent to TLS: in tests and dev this all runs over
`127.0.0.1`; in production the listener binds the Nebula interface address. **No
Nebula code, no real overlay, is needed to exercise the mTLS layer.**

## Identity & registration

### Cert issuance vs. registration (clean split)

`novactl node issue` and `/fed/v1/register` have **disjoint responsibilities** so
identity is decided at issue time and merely *materialized* at register time:

- **`novactl node issue --name <donor>`** generates a donor federation client
  cert + key and a `node-manifest.json`, **embeds a freshly-minted `node_id`
  UUID in the cert's URI SAN** (`nova://node/<uuid>`), and prints the fingerprint.
  It does **not** write the DB by default — the operator hands the bundle to the
  donor out-of-band.
- **`POST /fed/v1/register`** verifies the mTLS cert, reads the `node_id` UUID
  from the verified leaf's URI SAN, and on first contact **inserts
  `nodes.id = <that UUID>`** with the current DER fingerprint; thereafter it is
  idempotent on the fingerprint. A cert lacking the `nova://node/<uuid>` URI SAN
  (i.e. not issued by `novactl node issue`) is rejected.

`node_id` is thus **stable and operator-determined**, surviving cert rotation
(the replacement cert carries the same URI SAN). Revocation by `node_id` works
after registration. Pre-registration revocation (a revoked-fingerprints /
pending-invites table) is **not** built in M2 — added later only if needed.

### Request shape & cross-checks

`wire.RegisterRequest` is extended **additively** to the full normative shape:
negotiation inputs (`supported_protocols`, `capabilities`, `client_version`) plus
self-declared attributes (`display_name`, `geo_declared`, `capacity_bytes`,
`bandwidth_budget_bytes_per_day`, `policy_filters`) and the two reported
fingerprints (`nebula_cert_fingerprint`, `federation_cert_fingerprint`).

- **`federation_cert_fingerprint`** is **verified** against the mTLS peer leaf;
  a mismatch is rejected. This is the source of Nova application identity.
- **`nebula_cert_fingerprint`** is accepted as operator-issued / donor-reported
  **metadata only**. M2 does **not** import or parse Nebula certs, so it cannot
  cryptographically prove this matches the actual overlay cert; it may be compared
  against an operator-provided expected value but is **never** treated as identity.
- `geo_declared` is informational; placement anti-affinity (M5) uses the
  operator-verified `failure_domain_id`, which does not exist yet.

### Capability negotiation (fail-closed, but honest)

The negotiation **machinery** ships in M2 (it already exists as
`wire.NegotiateCapabilities`), but M2 must **not advertise capabilities it does
not implement.** `fed/v1` register + heartbeat are the **base protocol**, not
optional capabilities. Therefore in M2:

- The donor sends `supported_protocols=["fed/v1"]` and `capabilities=[]` (it does
  **not** claim `pin-change-log/v1`, `snapshot/v1`, `repair-stream/v1`, or
  `audit-block-hash/v1` — those endpoints don't exist yet).
- The coordinator selects `fed/v1` and replies `required_capabilities=[]`.
- Negotiation passes trivially; the persisted `advertised_capabilities` /
  `required_capabilities` are honestly empty.
- M3 begins **requiring** `pin-change-log/v1` + `snapshot/v1`; M4 adds
  `repair-stream/v1`; M6 adds `audit-block-hash/v1` — each as its endpoints land.

**Refusal is non-2xx.** No compatible protocol → `400 Bad Request` with
`{"code":"incompatible_protocol"}`; a missing required capability → `400` with
`{"code":"missing_capability"}`. (`201 Created` means success only.) The
negotiation machinery + negative tests ship now even though the M2 required set
is empty.

### Persisted registration state

A successful first register inserts a `nodes` row with `status='active'`,
`trust_state='probationary'`, the negotiated `selected_protocol`, the
`advertised_capabilities` / `required_capabilities` arrays, `client_version`, the
declared capacity/budget/policy, and the DER fingerprint. Re-register returns
`200` + the existing `node_id`.

## Heartbeat

`POST /fed/v1/heartbeat` (mTLS, identity from the verified cert) updates
`last_seen_at`, `last_free_bytes`, `last_stored_bytes`. Response matches the
normative shape:

```json
{ "config_updates": { "heartbeat_interval_seconds": 300, ... }, "current_epoch": 0, "repair_token_public_key": "" }
```

- `config_updates` carries the `federation` timers so a donor can be retuned
  without redeploy.
- `current_epoch` is a static `0` until M3's change log gives it meaning.
- `repair_token_public_key` is **empty** (M4 populates it).

Heartbeat **records** liveness; it does **not** run the liveness sweeper. The
timer-driven `active→suspect→unreachable→evicted` transitions are M5. In M2 a
registered node stays `active`.

## Cert lifecycle (`novactl node`)

`ca-init` / `issue` / `nebula-template` are **local file operations only** (no DB,
no API). `revoke` / `rotate-cert` / `list` mutate/read the registry **directly via
`DATABASE_URL`** — the same pattern `novactl setup` already uses, because **no
node admin API exists in M2** (operational mutations elsewhere use the admin API,
but standing up an authenticated `/api/v1/admin/nodes` surface + a node dashboard
is not load-bearing for M2 and is a natural later addition). Enforcement does not
depend on the API: the federation server reads `nodes.status` / the stored
fingerprint **live** on every request, so a direct write takes effect at once.

| Command | Behavior |
|---|---|
| `node ca-init` | Generate the federation X.509 CA (`federation-ca.crt` / `federation-ca.key` / `federation-ca.manifest.json`) **and** the coordinator's federation **server** cert (`coordinator-federation.crt` / `.key`). Explicit names — never a generic `ca.crt` (the donor bundle has two trust roots). Local files only. |
| `node issue --name <d>` | Donor federation client cert + key + `node-manifest.json`; mints + embeds `node_id` UUID in the URI SAN (`nova://node/<uuid>`); prints the DER fingerprint. Local files only, **no DB write**. |
| `node revoke <id>` | DB-direct `UPDATE nodes SET status='revoked', cert_revoked_at=now() WHERE id=$1`. The federation handler then **fails closed** for that node (see Revocation enforcement). The `federation.node_revoked` **webhook is deferred to M5** — `internal/webhook` is an empty placeholder today (no dispatcher exists), and M5 owns the federation webhook wiring. The poison-pill `pin_assignments` purge + re-replication is M4/M5 (none exist in M2). |
| `node rotate-cert <id>` | Issue a replacement cert with the **same** `node_id` URI SAN + a new key, compute its new DER fingerprint, then DB-direct `UPDATE nodes SET federation_cert_fingerprint=<new>, cert_rotation_started_at=now(), cert_rotated_at=now() WHERE id=$1`. Immediate cutover (see Cert rotation). |
| `node list` | DB-direct `SELECT` of the registry (id, display_name, status, trust_state, selected_protocol, last_seen_at) for operator visibility. |
| `node nebula-template --name <d> --nebula-ip <ip>` | Emit Nebula `config.yml` + operator README + donor `node.yaml` + a compose fragment, with explicit `nebula-ca.crt`/`nebula.crt`/`nebula.key` vs `federation-ca.crt`/`federation.crt`/`federation.key`, lighthouse/bind placeholders, and the exact `nebula-cert` commands to run externally. **No shell-out, no Nebula import.** Local files only. |

### Revocation & identity enforcement (handler-level authorization)

Revocation is a **handler-level authorization check**, not TLS-handshake PKI
(no CRL/OCSP — a revoked cert is still CA-valid). After a successful mTLS
handshake the handler:

1. Extracts the verified leaf → `node_id` (URI SAN) + DER `fingerprint`.
2. `SELECT … FROM nodes WHERE id = node_id`.
   - **Not found** → on `/register`, first-contact insert; on `/heartbeat`,
     `403` (`registration_required`).
   - **`status='revoked'`** → `403` (`node_revoked`).
   - **stored `federation_cert_fingerprint` ≠ presented `fingerprint`** → `403`
     (`fingerprint_mismatch`) — an old / un-activated cert (the rotation cutover).
   - else → proceed (register idempotent `200`; heartbeat updates `last_seen_at`).

Lookup is keyed on **`node_id` (the stable URI-SAN UUID)**, with the stored
fingerprint as the authoritative *current* cert — so rotation (same `node_id`,
new fingerprint) and an old cert (same `node_id`, stale fingerprint) are
distinguishable. Nebula-cert revocation (overlay membership) stays operator-driven
via the lighthouse, documented in the template README.

### Cert rotation — M2 is downtime cutover

`novactl node rotate-cert` issues the new cert **and immediately swaps the stored
fingerprint** in the DB (operator-driven activation). Until the operator delivers
the new bundle and restarts the donor, the donor's old cert hits
`fingerprint_mismatch` `403` — the accepted **downtime cutover**. Zero-downtime
overlap would need `pending_`/`previous_federation_cert_fingerprint` +
`cert_rotation_expires_at` (or a `node_certificates` table); M2 does **not** build
that. The rotation test asserts old-fails / new-works / `node_id`-unchanged
(rotation must never look like a second node registering).

## Donor side

- **`internal/node/agent`** becomes a real loop: load durable state; if
  unregistered, `register`; then `heartbeat` every `heartbeat_interval_seconds`
  (honoring `config_updates`), with bounded backoff on transport failure. **No
  `pins/changes` poll** (M3).
- **`internal/federation/transport` use:** the donor builds its mTLS client from
  `federation_cert_path` / `federation_key_path` + `federation_ca_path`, reading
  the PEM **at the configured `node.yaml` paths** (`os.ReadFile`) — these may point
  at `/run/secrets/*` mounts, which is how the donor honors the secret-mount model.
  (`secret.ResolveSecret` is *not* used here: it resolves env-keyed secret
  *contents*, not the path-based `*_path` references the M1 design itself flagged
  it is the wrong tool for. The donor may grow env/`_FILE` overrides later; M2 is
  path-based.)
- **`internal/node/state`** gains a `RegistrationStore` (kept **separate** from
  the existing cursor/jti `Store` interface, which stays an M3/M4 seam):

  ```go
  type RegistrationStore interface {
      LoadRegistration(context.Context) (Registration, bool, error)
      SaveRegistration(context.Context, Registration) error
  }
  ```

  A file-backed implementation writes `storage_dir/state/registration.json`
  atomically (temp → fsync → rename → dir-fsync, `0600`). `MemStore` stays for
  tests.
- **`cmd/node`** wires the real agent + transport; `--validate` / `--healthcheck`
  are unchanged.

## Schema — migration `0011_node_registration.sql`

```sql
ALTER TABLE nodes
  ADD COLUMN trust_state text NOT NULL DEFAULT 'probationary'
    CHECK (trust_state IN ('probationary', 'trusted', 'suspended')),
  ADD COLUMN selected_protocol       text,
  ADD COLUMN advertised_capabilities text[] NOT NULL DEFAULT '{}',
  ADD COLUMN required_capabilities   text[] NOT NULL DEFAULT '{}',
  ADD COLUMN client_version          text,
  ADD COLUMN cert_revoked_at          timestamptz,
  ADD COLUMN cert_rotation_started_at timestamptz,
  ADD COLUMN cert_rotated_at          timestamptz,
  ADD COLUMN last_free_bytes   bigint CHECK (last_free_bytes   IS NULL OR last_free_bytes   >= 0),
  ADD COLUMN last_stored_bytes bigint CHECK (last_stored_bytes IS NULL OR last_stored_bytes >= 0);
```

`trust_state` is `text + CHECK` (not a Postgres enum): the classification is young
and still evolving, and a text+CHECK domain is far cheaper to amend than
`ALTER TYPE`. **Omitted:** `last_heartbeat_error` (the coordinator cannot observe
donor-side transport failures and there is no concrete writer for it in M2) and
all D8 failure-domain / `placement_weight` / `pin_changes` / `assignment_id` /
`generation` / `blob_replication_state` / audit columns (M3/M5/M6). New
`internal/db/queries/federation.sql` (upsert-on-register, heartbeat update,
revoke, rotate, lookup-by-fingerprint) feeds sqlc via `make sqlc-generate`. The
new file is appended to `MANIFEST.sha256`; no shipped migration is touched.

## Configuration

Operator `federation` block (extends the existing timer-only struct in
`internal/config/types.go`):

```yaml
federation:
  listen_addr: "10.42.0.1:9443"          # authoritative bind (coordinator Nebula overlay addr)
  nebula_interface: "nebula1"            # validation guard (see below)
  federation_ca_path:   "/etc/nova/federation/federation-ca.crt"
  federation_cert_path: "/etc/nova/federation/coordinator-federation.crt"
  federation_key_path:  "/etc/nova/federation/coordinator-federation.key"
  heartbeat_interval_seconds: 300        # existing
  pins_poll_interval_seconds: 600        # existing
  max_pin_concurrency: 16                # existing
```

- `listen_addr` is **authoritative**. `nebula_interface`, when set, is a
  **startup validation guard**: the resolved listen IP must belong to that
  interface — except in dev/test loopback mode. This catches the
  accidental-`0.0.0.0` foot-gun (a donor cert authenticating over the public
  internet) at boot.
- Env overrides mirror the M0.4 pattern. `repair_token_ttl_seconds` is **not**
  added in M2 — M4 adds it alongside token minting.
- The donor `node.yaml` already carries the cert `*_path` fields from M1; M2
  starts *reading* them.

## Testing

- **Headline — loopback mTLS integration test.** A real
  `internal/federation/coordinator` server + donor agent over `127.0.0.1` with a
  test-generated federation CA: register → stable `node_id` → idempotent
  re-register → heartbeat updates `last_seen_at` → capability mismatch is
  fail-closed `400` → protocol mismatch is fail-closed `400` → revoked cert
  rejected at the next handshake. **No real Nebula.**
- **Public-mux exclusion.** Assert `POST /fed/v1/register` on the **public**
  coordinator handler is `404` (reservation ≠ a served federation endpoint).
- **Rotation/cutover.** Old cert registers → issue + activate new cert → new cert
  succeeds → old cert fails → `node_id` unchanged.
- **Units.** DER fingerprint + URI-SAN `node_id` extraction; capability/protocol
  negotiation + persistence; registration idempotency; donor JSON
  `RegistrationStore` atomic round-trip; `novactl node ca-init/issue/revoke/rotate`.
- **Boundary.** `donor-deps-boundary` stays green. The donor now imports the new
  shared `internal/federation/transport` (pure stdlib `crypto/tls`/`crypto/x509`)
  and begins *using* the already-allowlisted `internal/secret`. The allowlist in
  `scripts/check_node_deps.sh` currently permits `internal/federation/wire`
  **specifically**, not the broader `internal/federation` prefix, so M2 makes one
  **deliberate, reviewed allowlist edit** — add `internal/federation/transport`
  (the script comment explicitly anticipates this) — and the gate must be shown
  green afterward (and red against a reverted operator-only import, as in M1).
  `go build ./... && go vet ./...` green; coordinator smoke unaffected.

## Exit criteria

1. A donor with operator-issued certs registers over mTLS, gets a **stable
   `node_id`** + negotiated `fed/v1`, lands `trust_state='probationary'`,
   `status='active'`, and heartbeats; `last_seen_at` advances.
2. An incompatible donor (no protocol overlap, or missing a required capability)
   is **refused fail-closed** with a non-2xx machine-readable error.
3. A **revoked** cert is rejected at the next handshake; rotation preserves
   `node_id`.
4. Identity derives from the **verified federation cert** (DER fingerprint +
   URI-SAN UUID), never from request JSON or Nebula metadata.
5. `/fed/v1/*` is served **only** on the federation listener; it is `404` on the
   public/admin mux.
6. `donor-deps-boundary` green; `migrations-frozen` green with the new `0011`;
   `go build ./... && go vet ./...` green; coordinator smoke unchanged.
7. **No pins, no transfers, no liveness sweeper, no audits** — those remain
   M3–M6.

## Gotchas & deferrals

- **Bind both listeners before declaring success.** A federation-listener bind
  failure must not leave the public coordinator silently up.
- **Refusals are non-2xx.** `400` + `{"code": …}`, never a refused `201`.
- **DER, not SPKI.** The fingerprint tracks the certificate so rotation/revocation
  bookkeeping is correct; identity continuity comes from the URI-SAN UUID.
- **Don't lie about capabilities.** M2 advertises/requires an empty capability
  set; the future constants stay defined but unadvertised until their endpoints
  exist.
- **No Nebula in Go.** No library import, no `nebula-cert` shell-out in core
  flows; the overlay is transparent to the mTLS layer and absent from tests.
- **Frozen migrations.** Append `0011`; never edit `000*`.
- **Repair-token signer, liveness sweeper, failure-domain/placement columns,
  trust graduation, donor repair endpoint** — all explicitly later (M4/M5/M6).

## Cross-references

- Master design + plan:
  [`2026-06-11-phase2-federation-design.md`](2026-06-11-phase2-federation-design.md),
  [`../../plans/phase2/2026-06-11-phase2-federation.md`](../../plans/phase2/2026-06-11-phase2-federation.md).
- P2-M1 design (the seams this milestone fills):
  [`2026-06-15-phase2-m1-build-repo-separation-design.md`](2026-06-15-phase2-m1-build-repo-separation-design.md).
- Normative contracts (P2-M0-amended): `docs/specs/FEDERATION_PROTOCOL.md`
  (register/heartbeat/identity-from-mTLS/capability negotiation),
  `docs/specs/DATA_MODEL.sql` (`nodes`), `docs/specs/ARCHITECTURE_DECISIONS.md`,
  `docs/specs/KUBO_HARDENING.md`.
- Donor operations: `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`,
  `docs/quickstart/donor.md` (digest-pin + `cosign verify` mature at P2-M7).
- This milestone's plan:
  [`../../plans/phase2/2026-06-16-phase2-m2-identity-registration.md`](../../plans/phase2/2026-06-16-phase2-m2-identity-registration.md).
