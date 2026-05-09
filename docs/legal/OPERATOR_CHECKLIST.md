# Operator Checklist

A pre-launch and ongoing checklist for site operators running a Nova
coordinator. Items marked **[REQUIRED]** must be complete before
public launch; items marked **[RECOMMENDED]** may be deferred but are
strongly suggested.

**Disclaimer.** This document is engineering and operational guidance,
not legal advice. The legal items below describe the architectural
features Nova provides to support compliance; whether any given
operator achieves compliance depends on their jurisdiction, their
conduct, and the advice of their counsel.

## Pre-deployment

### Read the foundational documents

- [ ] [REQUIRED] Read [`docs/THREAT_MODEL.md`](../THREAT_MODEL.md) end-to-end. Make sure the residual risks are acceptable for your community.
- [ ] [REQUIRED] Read [`docs/PRIVACY_AUDIT.md`](../PRIVACY_AUDIT.md). Decide whether to enable `paranoid: true` for your deployment.
- [ ] [REQUIRED] Read [`docs/specs/ENCRYPTION_ENVELOPE.md`](../specs/ENCRYPTION_ENVELOPE.md). Understand that the operator master key is the single most critical secret in the deployment.

### Engage counsel

- [ ] [REQUIRED] Consult counsel in the jurisdictions where the coordinator and your users are located. Topics to cover:
  - Whether your service qualifies for DMCA Section 512 (or equivalent) safe harbor.
  - Your obligations under GDPR / CCPA / your local privacy regime.
  - Record-retention requirements for takedowns and audit logs.
  - Indemnification language for the Terms of Service.

### Designated agent registration

- [ ] [REQUIRED for US-jurisdiction operators] Register a designated agent with the U.S. Copyright Office.
  - Filing portal: <https://dmca.copyright.gov/>
  - Registration is per-service-provider, not per-domain.
  - Three-year renewal cycle.
  - The agent's contact information must appear in your Terms of Service.

### Terms of Service and Privacy Policy

- [ ] [REQUIRED] Adapt [`TOS_TEMPLATE.md`](TOS_TEMPLATE.md) for your service. Have counsel review.
- [ ] [REQUIRED] Publish your Terms of Service at a stable URL (e.g., `/terms`).
- [ ] [REQUIRED] Set `tos_url` in `operator.yaml`. The coordinator refuses to accept public uploads without it.
- [ ] [REQUIRED] Publish a Privacy Policy that describes:
  - What data you collect (including `source_ip`, retention period).
  - The donor-blind storage architecture (referenced from PRIVACY_AUDIT.md).
  - Your DSAR contact and procedure.

## Master key handling

- [ ] [REQUIRED] Generate a 256-bit operator master key from a CSPRNG. Example:
  ```sh
  openssl rand -hex 32
  ```
- [ ] [REQUIRED] Store the key in `NOVA_MASTER_KEY` environment variable. **Never** commit it to a repository, write it to a Dockerfile, or log it.
- [ ] [REQUIRED] Back the key up out-of-band. Recommended: print on paper, store in a safe; or split with Shamir's Secret Sharing across trusted parties.
- [ ] [REQUIRED] Test the recovery procedure before going live. Restore the key from your backup into a clean test deployment, decrypt a test blob, and verify success.
- [ ] [RECOMMENDED] Plan annual master-key rotation; document the procedure in your runbook.

**Loss of the master key is permanent loss of every blob in the
federation.** This is the single most consequential operational
risk; plan accordingly.

## TLS configuration

- [ ] [REQUIRED] Choose a TLS mode (see [`PRIVACY_AUDIT.md`](../PRIVACY_AUDIT.md#tls-mode-and-ct-log-exposure)):
  - `quick-setup` (HTTP-01): convenient, leaks hostname to public CT logs forever.
  - `dns-01-wildcard`: hostname not in CT logs; needs DNS API access.
  - `static`: operator-supplied cert.
  - `.onion`: self-signed cert behind a Tor hidden service.
- [ ] [REQUIRED] Confirm certificate auto-renewal is configured and tested.

## Hardening verification

- [ ] [REQUIRED] Confirm `IPFS_SWARM_KEY` is set and unique to your federation.
- [ ] [REQUIRED] Confirm Kubo is running with the validator-approved configuration. The donor and coordinator binaries refuse to start otherwise; verify the boot logs show a successful validation.
- [ ] [REQUIRED] Confirm the coordinator container does not run as root.
- [ ] [REQUIRED] Confirm `auth: anonymous` and `moderation: off` are not set simultaneously. The coordinator refuses such a configuration; verify by inspecting your deployed config.
- [ ] [RECOMMENDED] Enable `paranoid: true` for any deployment that does not have a specific reason to allow phone-home behaviour.

## Federation diversification

> The most consequential single architectural rule for federation
> resilience.

The orchestrator simulation (`simulations/orchestrator_resilience.py`)
shows that a "single hosting provider purges everyone" event takes
**5–10× longer to recover from** than a uniform-random failure of the
same magnitude. The mitigation lives at the operator level, not in
software.

- [ ] [REQUIRED] When recruiting high-bandwidth-VPS donors, ensure the cohort is distributed across **at least three distinct hosting providers**. The orchestrator cannot enforce this; the operator must monitor it.
- [ ] [RECOMMENDED] Maintain a public list of "providers in good standing" (e.g., on the supporters page, with `paranoid: true` off) to encourage donors to spread out organically.
- [ ] [RECOMMENDED] If a single provider exceeds 40 % of total federation capacity, post in the operator's channel (forum / chat / mailing list) asking new donors to choose a different provider.

## Moderation policy

- [ ] [REQUIRED] Decide your moderation policy:
  - PDQ blocklist sources (StopNCII is a good baseline).
  - PDQ similarity threshold (default Hamming distance ≤ T = 31, configurable).
  - Repeat-infringer strikes (default 3).
  - Quarantine duration before automatic tombstone (default 30 days).
- [ ] [REQUIRED] Designate moderators in your team. Assign the `moderator` role in `users.role`.
- [ ] [REQUIRED] Establish your moderation SLA — how quickly will the team review the queue?
- [ ] [RECOMMENDED] Document the policy publicly so users know what is and is not permitted.

## DSAR (data-subject access request) handling

- [ ] [REQUIRED] Designate a DSAR contact (typically counsel or a privacy officer).
- [ ] [REQUIRED] Update `/legal/dsar` with your contact and instructions.
- [ ] [REQUIRED] Document your internal DSAR procedure: how a request reaches the team, who authenticates the requester, who runs the query, who reviews the response.
- [ ] [REQUIRED] Test the DSAR path with a synthetic user before launch.

## Backups

- [ ] [REQUIRED] Postgres: daily logical dump (`pg_dump`) plus continuous WAL archiving to off-host storage.
- [ ] [REQUIRED] Test Postgres restore from backup before launch.
- [ ] [RECOMMENDED] Operator master-key backup tested as a separate procedure (above).
- [ ] [RECOMMENDED] Audit-log retention sized to your jurisdiction's record-retention requirements.

## Operational readiness

- [ ] [REQUIRED] Configure metrics scraping. Prometheus endpoint defaults to loopback; either bind to a private interface or run a scraper inside the same trust boundary.
- [ ] [REQUIRED] Configure log shipping. Logs are written to stdout; ship them to your log aggregator at the host level.
- [ ] [REQUIRED] Define alerts for: coordinator down, Postgres replication lag, donor mass-casualty events (`federation.degraded` webhook), PDQ scan match rate spike (could indicate brigading).
- [ ] [RECOMMENDED] Run a 24-hour load test before public launch.
- [ ] [RECOMMENDED] Run a chaos test (Pumba kills a random donor every N minutes) and verify orchestrator restores replication within SLA.

## Post-launch

### Ongoing

- [ ] Monitor donor diversification ratios; recruit to under-represented providers.
- [ ] Process the moderation queue within your declared SLA.
- [ ] Process DMCA notices within your declared SLA (Nova's takedown action is fast; the human review is the bottleneck).
- [ ] Review the audit log monthly for unexpected privileged actions.
- [ ] Rotate signing keys (HMAC for signed URLs) at your declared cadence (default every 90 days; `POST /api/v1/admin/keys/rotate-signing`).

### Annually

- [ ] Re-read this checklist; flag items that have drifted from current practice.
- [ ] Renew DMCA agent registration if the three-year window approaches.
- [ ] Refresh consultation with counsel if jurisdictional law has changed.
- [ ] Rotate the operator master key.
- [ ] Verify backups still restore cleanly.

## On incident

- [ ] Suspected coordinator compromise: immediately rotate the master key (re-wraps every per-blob key with the new master key); review the audit log; consider taking the coordinator offline pending investigation.
- [ ] Suspected donor compromise: `novactl node revoke <id> --reason "incident-N"`; the orchestrator handles re-replication automatically.
- [ ] Suspected master-key compromise: rotate the master key immediately. Treat any data the suspected attacker may have accessed as compromised.
- [ ] Suspected signed-URL key compromise: rotate via `POST /api/v1/admin/keys/rotate-signing`, set the grace window to 0, and revoke `kid:{old_kid}` to invalidate every URL signed with the leaked key.

## Things Nova cannot do for you

- Defend a poorly-secured host operating system.
- Replace a host-based intrusion detection system.
- Provide legal advice or jurisdiction-specific compliance guarantees.
- Recover from operator master-key loss.
- Run highly-available across multiple coordinators (Phase 1–5 is single-coordinator).
- Decrypt content for law-enforcement purposes by design (and not because of refusal — the coordinator literally lacks the keys to decrypt content whose blob keys have been crypto-shredded).
