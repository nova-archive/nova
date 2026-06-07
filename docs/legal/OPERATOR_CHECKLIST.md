# Operator Checklist

A pre-launch and ongoing checklist for site operators running a Nova
coordinator. v2 narrows the [REQUIRED] set to only what the
coordinator literally cannot run safely without; everything else is
[RECOMMENDED] with operator judgment.

**Disclaimer.** This document is engineering and operational
guidance, not legal advice. The legal items below describe the
architectural features Nova provides to support compliance; whether
any given operator achieves compliance depends on their
jurisdiction, their conduct, and the advice of their counsel.

## What [REQUIRED] means in v2

The coordinator's startup health checks already enforce a set of
non-negotiable safety floors (refuse-to-start rules — see
`docs/PRIVACY_AUDIT.md` § "Hardening defaults"). [REQUIRED] in this
checklist means an item the coordinator cannot run without — either
because the binary refuses to start, or because the deployment
becomes operationally unrecoverable without it (e.g., losing the
master key without a backup is permanent data loss for the entire
federation).

[RECOMMENDED] items are operator judgment calls. Small private
communities running Nova for a friend group may reasonably skip
many of them; high-profile public deployments with paying users or
substantial liability exposure should treat most [RECOMMENDED]
items as effectively required for their threat profile and
consult counsel.

## [REQUIRED] — coordinator cannot run safely without these

### 1. Master key generation, storage, and backup

- [ ] [REQUIRED] Generate a 256-bit operator master key from a CSPRNG:
  ```sh
  openssl rand -hex 32
  ```
- [ ] [REQUIRED] Store the key in the `NOVA_MASTER_KEY` environment
      variable. **Never** commit it to a repository, write it to a
      Dockerfile, or log it.
- [ ] [REQUIRED] Back the key up out-of-band. Recommended: print on
      paper and store in a safe; or split with Shamir's Secret
      Sharing across trusted parties; or use a secret manager
      (HashiCorp Vault, 1Password, etc.) with offline backup.
- [ ] [REQUIRED] **Test recovery before going live.** Restore the
      key from your backup into a clean test deployment, decrypt a
      test blob, and verify success. An untested backup is no backup.

**Loss of the master key is permanent loss of every blob in the
federation.** This is the single most consequential operational
risk; the coordinator cannot decrypt any user content without the
key it was encrypted under, and there is no recovery mechanism.

### 2. Hardening floors (the coordinator refuses to start without these)

These are the refuse-to-start rules from `docs/PRIVACY_AUDIT.md`.
The coordinator and donor binaries enforce them; the checklist is
to verify they are satisfied in your deployment.

- [ ] [REQUIRED] `IPFS_SWARM_KEY` environment variable is set with
      a 256-bit hex secret unique to your federation, **OR**
      `public_ipfs_dht: true` is set in operator config (deliberate
      opt-out for public-archive deployments).
- [ ] [REQUIRED] Kubo daemon is running with the validator-approved
      configuration (`Bootstrap` empty or operator-controlled,
      `Routing.Type=none` in private mode, `Discovery.MDNS.Enabled=false`,
      empty `Provider.Strategy` and `Reprovider.Strategy`,
      loopback-only API and Gateway, `Swarm.DisableNatPortMap=true`).
      The donor and coordinator binaries refuse to start otherwise;
      verify by inspecting the boot logs for the validator's success
      message.
- [ ] [REQUIRED] Coordinator container does not run as root.
- [ ] [REQUIRED] No simultaneous `auth: anonymous` and
      `moderation: off` setting; coordinator refuses to start in
      that combination.

### 3. Postgres backup configured and tested

- [ ] [REQUIRED] Postgres: configure either daily logical dump
      (`pg_dump`) or continuous WAL archiving to off-host storage.
- [ ] [REQUIRED] Test Postgres restore from backup before launch.
      A coordinator with `nodes`, `pin_assignments`, and
      `data_encryption_keys` rows lost is a non-functional
      deployment regardless of what is on donor disks.

### 4. TLS configured (only if exposing the public hostname)

- [ ] [REQUIRED if exposing publicly] Choose a TLS mode (see
      `docs/PRIVACY_AUDIT.md` § "TLS mode and CT log exposure"):
  - `quick-setup` (HTTP-01): convenient, leaks hostname to public
    CT logs forever.
  - `dns-01-wildcard`: hostname not in CT logs; needs DNS API access.
  - `static`: operator-supplied cert.
  - `.onion`: self-signed cert behind a Tor hidden service.
- [ ] [REQUIRED if exposing publicly] Confirm certificate
      auto-renewal is configured and tested.

This item is [RECOMMENDED] (not required) if your coordinator is
not reachable from the public internet — for example, a homelab
serving only friends over Tailscale or Nebula, or an internal
corporate deployment.

---

## [RECOMMENDED] — operator judgment for your threat profile

### Foundational reading

- [ ] [RECOMMENDED] Read [`docs/THREAT_MODEL.md`](../THREAT_MODEL.md)
      end-to-end. Make sure the residual risks are acceptable for
      your community.
- [ ] [RECOMMENDED] Read [`docs/PRIVACY_AUDIT.md`](../PRIVACY_AUDIT.md).
      Decide whether to enable `paranoid: true` for your deployment.
- [ ] [RECOMMENDED] Read [`docs/specs/ENCRYPTION_ENVELOPE.md`](../specs/ENCRYPTION_ENVELOPE.md)
      to understand the master-key criticality.

### Counsel and legal posture

- [ ] [RECOMMENDED, ESPECIALLY FOR PUBLIC DEPLOYMENTS] Consult
      counsel in the jurisdictions where the coordinator and your
      users are located. Topics worth covering: DMCA Section 512
      safe harbor, GDPR/CCPA obligations, record-retention,
      indemnification language for ToS.

      Small private deployments (friend groups, hobby communities)
      may reasonably skip formal legal review and rely on the
      enforcement primitives Nova ships with (DMCA quarantine flow,
      audit logging, signed-URL revocation). Public deployments
      with paying users or significant liability exposure should
      not skip this.

### Designated DMCA agent (US deployments hosting public uploads)

- [ ] [RECOMMENDED FOR US-JURISDICTION OPERATORS HOSTING PUBLIC
      UPLOADS] Register a designated agent with the U.S. Copyright
      Office at <https://dmca.copyright.gov/>. Three-year renewal.
      The agent's contact information must appear in your Terms
      of Service if you publish one.

### Terms of Service and Privacy Policy

- [ ] [RECOMMENDED] Adapt [`TOS_TEMPLATE.md`](TOS_TEMPLATE.md) for
      your service. Have counsel review if the deployment scale
      justifies it.
- [ ] [RECOMMENDED] Publish your Terms of Service at a stable URL
      (e.g., `/terms`).
- [ ] [RECOMMENDED] Set `tos_url` in `operator.yaml` if publishing
      a ToS. Note: the coordinator refuses to accept *anonymous*
      public uploads without `tos_url` set, but authenticated
      uploads do not require it.
- [ ] [RECOMMENDED] Publish a Privacy Policy describing the data
      you collect and your retention windows.

### Federation diversification

- [ ] [RECOMMENDED] When recruiting high-bandwidth-VPS donors,
      distribute the cohort across at least three distinct hosting
      providers. The orchestrator simulation shows that single-
      provider purges multiply recovery time by 5–10× compared to
      uniform-random failures of the same magnitude.
- [ ] [RECOMMENDED] If a single provider exceeds 40 % of total
      federation capacity, post in the operator's channel asking
      new donors to choose a different provider.

### Moderation policy

- [ ] [RECOMMENDED] Decide your moderation policy:
  - PDQ blocklist sources (StopNCII is a good baseline if you want
    severe-content protection).
  - Default takedown action: `quarantine` (default, reversible
    during counter-notification window) or `tombstone` (immediate,
    irreversible).
  - Repeat-infringer strikes (default 3).
- [ ] [RECOMMENDED] Designate moderators in your team. Assign the
      `moderator` role in `users.role`.
- [ ] [RECOMMENDED] Establish your moderation SLA — how quickly will
      the team review the queue?

Small private communities may reasonably defer all of this to a
single operator account that handles moderation manually as
incidents arise.

### DSAR (data-subject access request) handling

- [ ] [RECOMMENDED] Designate a DSAR contact (typically counsel or
      a privacy officer for organizations).
- [ ] [RECOMMENDED] Update `/legal/dsar` with your contact and
      instructions.
- [ ] [RECOMMENDED] Document your internal DSAR procedure.

### Operational readiness

- [ ] [RECOMMENDED] Configure metrics scraping. Prometheus endpoint
      defaults to loopback; either bind to a private interface or
      run a scraper inside the same trust boundary.
- [ ] [RECOMMENDED] Configure log shipping. Logs are written to
      stdout; ship them to your log aggregator at the host level.
- [ ] [RECOMMENDED] Define alerts for: coordinator down, Postgres
      replication lag, donor mass-casualty events
      (`federation.degraded` webhook), PDQ scan match rate spike.
- [ ] [RECOMMENDED] Run a 24-hour load test before public launch.
- [ ] [RECOMMENDED] Run a chaos test (Pumba kills a random donor
      every N minutes) and verify orchestrator restores replication
      within SLA.

---

## Post-launch (RECOMMENDED, ongoing)

- Monitor donor diversification ratios; recruit to under-represented
  providers.
- Process the moderation queue within your declared SLA.
- Process DMCA notices within your declared SLA (Nova's takedown
  action is fast; the human review is the bottleneck).
- Review the audit log monthly for unexpected privileged actions.
- Rotate signing keys (HMAC for signed URLs) at your declared
  cadence (default every 90 days; `POST /api/v1/admin/keys/rotate-signing`).

## Master-key rotation runbook

Use this procedure when you want to replace the active master key (e.g. on
schedule, after suspected compromise, or for operational hygiene). The
coordinator stays online throughout; reads work against either version during
the drain.

**⚠ CRITICAL: Back up EVERY master-key version out-of-band before proceeding.**
Loss of all versions = permanent, unrecoverable loss of every blob in the
federation. This applies to the new version too. An untested backup is no
backup — test restoration on a clean environment before relying on it.

### Five-step procedure

1. **Generate v2 and store it out-of-band.**
   ```sh
   openssl rand -hex 32 > /run/secrets/master-key-v2
   ```
   (Or use a secret manager. Store an off-box backup immediately.)

2. **Add v2 to the secret mount, keep v1, set active, restart.**
   - Both `NOVA_MASTER_KEY_V1` (or `_FILE` / default mount) and
     `NOVA_MASTER_KEY_V2` (or `_FILE` / default mount) must be present.
   - Set `NOVA_MASTER_KEY_ACTIVE=v2`.
   - Restart the coordinator. On boot it loads both versions; new uploads
     already wrap DEKs under v2. Old blobs still read (v1 is loaded).

3. **Trigger the rotation.**
   ```sh
   novactl keys rotate-master --from v1 --to v2
   ```
   The CLI prompts for confirmation (use `--no-confirm` to skip), then polls
   `GET /api/v1/admin/keys/rotation-status` printing remaining DEK and
   signing-key counts until the drain completes or stalls. The endpoint
   validates that `to_version` equals the active label; if not, it returns
   `400 to_not_active` and the restart from step 2 is missing.

4. **Wait for completion.**
   ```sh
   novactl keys status
   ```
   Wait until `v1` shows `state: retired` with `dek_count: 0` and
   `signing_count: 0`.

   **Do not remove v1 until this step confirms it.** If v1 is dropped while
   its drain is still in progress, the rotation stalls: the `v1` version stays
   `rotating`, `/readyz` degrades (readiness, not liveness — a restart cannot
   fix a missing key), and `rotation-status.stalled` becomes `true`. To
   recover, temporarily restore the v1 key and restart the coordinator.

5. **Drop v1 on the next deploy.**
   Once step 4 confirms `v1 retired, dek_count=0`, remove the v1 secret from
   mounts and env on your next deploy. Do not drop it before then.

### Autovacuum guidance for large deployments

Each DEK re-wrap is a Postgres `UPDATE` (delete + insert under MVCC), so
re-wrapping a million DEKs generates a million dead tuples. On large
deployments, consider:

- Temporarily setting a more aggressive `autovacuum_vacuum_scale_factor` on
  `data_encryption_keys` (e.g. `0.01` instead of the default `0.2`) for the
  duration of the rotation so autovacuum keeps up with the dead-tuple load.
  Restore the default after rotation completes.
  ```sql
  ALTER TABLE data_encryption_keys
    SET (autovacuum_vacuum_scale_factor = 0.01);
  -- ... after rotation ...
  ALTER TABLE data_encryption_keys
    RESET (autovacuum_vacuum_scale_factor);
  ```
- Setting `NOVA_MASTER_KEY_REWRAP_PACE_MS` higher (e.g. `200`–`500`) to
  smooth WAL and disk I/O on storage-constrained hosts. The default of 50 ms
  is conservative for most deployments; small instances may benefit from more
  breathing room.

## Admin SPA (operator console)

The M11 admin console (`web/admin/`) is a hermetic React + Vite bundle the
coordinator serves at `/admin/*`.

- **Enable it.** Build the bundle (`make admin-build`) and point
  `NOVA_ADMIN_DIST_DIR` at `web/admin/dist`. Unset ⇒ `/admin/*` stays
  unmounted (the console is disabled). In production (M13) nginx serves the
  bundle directly on the admin vhost instead.
- **Login modes.** With the built-in issuer, operators sign in with email +
  password; tokens live in the browser with silent refresh. With an external
  IdP (`auth.issuer_url`), the SPA drives the OIDC authorization-code + PKCE
  flow — ensure the IdP's CORS allows the admin origin (the coordinator's CSP
  already admits the configured issuer for the token exchange).
- **Owner soft-delete is irreversible after the grace window.** A blob deleted
  from the console (or `DELETE /api/v1/blobs/{cid}`) enters `soft_deleted`
  (reads return `410`); after `NOVA_SOFT_DELETE_GRACE_SECONDS` (default 86400)
  the in-process lifecycle sweep tombstones it and **crypto-shreds** the key —
  unrecoverable. Set the grace to your recovery window;
  `NOVA_SOFT_DELETE_SWEEP_ENABLED=false` pauses the sweep (soft-deletes then
  persist, reversibly, until re-enabled).
- The console only surfaces what the caller's role permits (server-enforced);
  key rotation is operator-only. Legal-hold clearance stays a `novactl` /
  Phase-4 action, not a console action.

## Annual maintenance (RECOMMENDED)

- Re-read this checklist; flag items that have drifted from current
  practice.
- Renew DMCA agent registration if applicable (three-year window).
- Refresh consultation with counsel if jurisdictional law has changed.
- Rotate the operator master key (follow the "Master-key rotation runbook"
  above; `POST /api/v1/admin/keys/rotate-master` is the API endpoint).
- Verify backups still restore cleanly.

## On incident (REQUIRED action when the incident occurs)

These items are unconditional: when the trigger condition occurs,
the action is required.

- [REQUIRED ON OCCURRENCE] Suspected coordinator compromise:
  immediately rotate the master key (re-wraps every per-blob key
  with the new master key); review the audit log; consider taking
  the coordinator offline pending investigation.
- [REQUIRED ON OCCURRENCE] Suspected donor compromise:
  `novactl node revoke <id> --reason "incident-N"`; the orchestrator
  handles re-replication automatically.
- [REQUIRED ON OCCURRENCE] Suspected master-key compromise:
  rotate the master key immediately. Treat any data the suspected
  attacker may have accessed as compromised.
- [REQUIRED ON OCCURRENCE] Suspected signed-URL key compromise:
  rotate via `POST /api/v1/admin/keys/rotate-signing`, set the
  grace window to 0, and revoke `(kind='kid', value=<old_kid>)` to
  invalidate every URL signed with the leaked key.

## Things Nova cannot do for you

- Defend a poorly-secured host operating system.
- Replace a host-based intrusion detection system.
- Provide legal advice or jurisdiction-specific compliance
  guarantees.
- Recover from operator master-key loss.
- Run highly-available across multiple coordinators (Phase 1–5 is
  single-coordinator).
- Decrypt content for law-enforcement purposes by design — the
  coordinator literally lacks the keys to decrypt content whose
  blob keys have been crypto-shredded. (See
  `docs/legal/SEVERE_CONTENT_PROCEDURE.md` for the legal-hold
  workflow that prevents accidental shredding of evidence.)
