# Severe Content Procedure

Status: **Phase 1 — manual operator path ships (M9). Full automation Phase 4.**

This document outlines the procedure for handling severe illegal content
(CSAM-class material) discovered on a Nova deployment. The **manual
operator path** — `novactl moderation quarantine --legal-hold` and the
operator-only `novactl moderation clear-legal-hold` — ships in M9. The
database schema (the `legal_hold` flag on `data_encryption_keys`,
`blob_state` including `quarantined`, `moderation_decisions.legal_hold`,
and the `no_shred_under_legal_hold` CHECK constraint) already enforces
the legal-hold gate at the storage layer. NCMEC integration, automated
detection, and the admin SPA legal-hold clearance UI remain Phase 4
deliverables.

This is intentional. Phase 1 is a single-node private MVP with
no public uploads from anonymous users; the immediate severe-
content surface is small. Phase 4 adds adapters and SDKs that
broaden public-upload exposure and is the appropriate point to
ship the full reporting workflow.

**Disclaimer.** This document is engineering and operational
guidance, not legal advice. Statutes are referenced for context
only. Operators of public-upload deployments must consult counsel
before relying on the architecture's severe-content handling.

## Why this is separate from DMCA

The DMCA quarantine-first procedure (`DMCA_PROCEDURE.md`)
schedules a tombstone + crypto-shred after a counter-notification
window. For routine copyright takedowns, that is the right
operational answer.

For severe illegal content (CSAM), automated crypto-shredding is
catastrophic. Under U.S. federal law:

- **18 U.S.C. § 2258A** imposes mandatory reporting obligations
  on Electronic Service Providers when they obtain "actual
  knowledge" of apparent child sexual exploitation. Reports go to
  the National Center for Missing & Exploited Children (NCMEC)
  CyberTipline.
- The **REPORT Act** (and predecessor statutes) mandates
  preservation of reported material — and the digital evidence
  surrounding it — for a statutory period (typically 90 days from
  the report, extendable on law-enforcement request to one year).
- Destroying evidence after acquiring actual knowledge can
  constitute obstruction and strips safe-harbor protections.

If a Nova deployment auto-shreds a per-blob key after a positive
PDQ match against StopNCII or a verified user report, the operator
has destroyed evidence they were legally required to preserve.

The architecture must therefore split severe-content handling
from DMCA. v2's `data_encryption_keys.legal_hold` flag and
`blob_state = 'quarantined'` together implement the correct
state: bytes preserved on donor disks, key preserved (not
shredded), public reads blocked, evidence retained for the
statutory period.

## Intended state machine (Phase 4 implementation)

```
  Detection
     │   (PDQ match against StopNCII, verified user report,
     │    or operator-initiated review)
     ▼
  moderation_decisions row:
     rule = 'severe_content'
     action = 'quarantine'
     legal_hold = true
     scheduled_tombstone_at = NULL    (CRITICAL: not auto-tombstoned)
     ▼
  blobs.state = 'quarantined'         (cascade to derivatives)
     ▼
  data_encryption_keys.legal_hold = true   (cascade to derivative keys)
     ▼
  signed_url_revocations (kind='cid', value=<cid>)
     ▼
  Public reads return 451             (Unavailable for Legal Reasons)
     ▼
  NCMEC CyberTipline report generated     (Phase 4)
     │
     ▼
  Statutory preservation window begins
     │   (90 days minimum; extendable on LE request)
     ▼
  Operator awaits law enforcement contact, if any
     │
     ▼
  After preservation window expires AND operator clears legal_hold:
     │
     ▼
  Standard tombstone procedure runs:
     legal_hold = false
     scheduled_tombstone_at = now()
     blobs.state = 'tombstoned'
     data_encryption_keys.state = 'shredded' (now permitted)
     federation unpin broadcast
     audit log
```

The critical architectural property: **`legal_hold = true`
prevents crypto-shred regardless of any other operation.** The
DB CHECK constraint
`CHECK (legal_hold = false OR state IN ('active', 'rotating'))`
on `data_encryption_keys` enforces this at the storage layer.

## Why crypto-shred is forbidden during preservation

The preservation window is a legal duty, not a policy choice. The
operator has obtained "actual knowledge" of severe content; they
must preserve the evidence surrounding it. This includes:

- The encrypted bytes on donor disks (mathematically tied to the
  per-blob key the operator holds).
- The per-blob key in `data_encryption_keys.wrapped_key`.
- The `blobs` row metadata.
- The audit log of who, when, how the content was detected.
- Surrounding context: the uploader's account, IP, timestamps.

Crypto-shredding the key would render the encrypted bytes
mathematically inert and destroy the operator's ability to produce
the evidence on law-enforcement subpoena. That is the destruction
of evidence the REPORT Act and § 2258A explicitly prohibit.

The operator can (and should) prevent further public access — and
v2 does this via `state = 'quarantined'` and the signed-URL
revocation entry — but the preservation duty is independent of
public access. Bytes preserved + access blocked is the legally
required posture during the preservation window.

## Operator legal-hold-clear procedure

After the statutory preservation window passes and the operator
has confirmed (typically via counsel and law-enforcement contact)
that no further preservation is required, the operator may clear
the hold (ships in M9; **operator role required**):

```sh
novactl moderation clear-legal-hold <cid> \
    --case-id <reference> \
    --reason "preservation period expired, no LE preservation request"
```

This command requires:

- Operator role (not moderator alone).
- A documented case reference (NCMEC report id or equivalent).
- Operator confirmation prompt.

After clearing:

```sql
UPDATE data_encryption_keys
   SET legal_hold = false
 WHERE id = <key_id>;

UPDATE moderation_decisions
   SET legal_hold = false,
       scheduled_tombstone_at = now()  -- triggers immediate tombstone
 WHERE id = <md_id>;
```

The standard scheduled-tombstone job then tombstones and
crypto-shreds on its next pass.

## Phase 1 scope (current)

Phase 1 (M9) includes:

- Database schema support: `legal_hold` flag on
  `data_encryption_keys`, `blob_state` includes `quarantined`,
  `moderation_decisions.legal_hold`, the `no_shred_under_legal_hold`
  CHECK constraint — **enforced at the DB layer** regardless of any
  application bug.
- Crypto-shred procedure refuses to run when `legal_hold = true`;
  the `no_shred_under_legal_hold` CHECK makes this a storage-layer
  guarantee (per `ENCRYPTION_ENVELOPE.md` § "Crypto-shredding").
- Manual quarantine with legal hold:
  `novactl moderation quarantine <cid> --legal-hold`.
- Operator-only legal-hold clearance:
  `novactl moderation clear-legal-hold <cid>` — enforces the operator
  role via the API guard (`403` for a moderator). After clearing, the
  next ≈1-minute sweep tombstones and shreds the blob.

Phase 1 does NOT include:

- Automated detection (PDQ scan against StopNCII at upload — Phase 3
  alongside other moderation hooks).
- NCMEC CyberTipline report generation — Phase 4.
- Legal-hold clearance UI in the admin SPA — Phase 4.
- Cross-jurisdictional reporting analogues (UK CEOP, EU INHOPE, etc.)
  — Phase 4+.

## What operators should do today

If you are running a Phase 1 deployment (private, single-node, no
anonymous public uploads) and a severe-content situation arises:

1. Immediately quarantine the blob:
   ```sh
   novactl moderation quarantine <cid> --reason "severe content review" --legal-hold
   ```
   This sets state to `quarantined`, marks the key for legal hold,
   and revokes outstanding signed URLs. It does NOT tombstone or
   shred.

2. Generate an NCMEC CyberTipline report manually
   (<https://report.cybertip.org/>) including the CID, upload
   timestamp, uploader account info from `users` and
   `audit_log`, and any other context.

3. Preserve the encrypted bytes and the key for the statutory
   period (do not run any tombstone, do not run any
   `clear-legal-hold` command, do not rotate the master key
   without preserving the old version).

4. Consult counsel regarding the specific incident.

5. After the preservation window and any LE preservation requests
   are resolved, run
   `novactl moderation clear-legal-hold <cid>`
   to release the hold and proceed with the standard tombstone.

This procedure is the manual analogue of what Phase 4 will
automate. Phase 1 deployments that handle public uploads must
treat severe-content handling as a manual operator responsibility.

## Implementation roadmap

| Phase | Severe-content capability |
|---|---|
| 0 | Schema + this doc; manual operator path documented |
| 1 ✅ | Schema-enforced `no_shred_under_legal_hold` CHECK (DB-layer guarantee); `novactl moderation quarantine --legal-hold`; operator-only `novactl moderation clear-legal-hold` (M9, tag `m9-moderation`) |
| 3 | Automated PDQ scan against StopNCII at upload, synchronous reject for clear matches; quarantine + legal_hold for ambiguous matches |
| 4 | NCMEC CyberTipline report generation, admin SPA legal-hold clearance UI, audit-log export for evidence packaging |
| 4+ | Multi-jurisdiction reporting (UK CEOP, EU INHOPE, etc.) |

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`legal_hold` columns,
  `blob_state` enum, no-shred-under-legal-hold CHECK constraint)
- Encryption: `docs/specs/ENCRYPTION_ENVELOPE.md` § "Crypto-shredding"
  (legal-hold gate)
- DMCA contrast: `docs/legal/DMCA_PROCEDURE.md`
- Threat model: `docs/THREAT_MODEL.md` § "Inducement / liability
  content uploader"
- Moderation: `docs/specs/PRODUCT_MODULE_INTERFACE.md` § "Reference:
  nova-image" (PDQ scanner integration)
