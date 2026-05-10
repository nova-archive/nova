# Takedown Procedure

The internal procedure for receiving, validating, processing, and
acting on takedown notices. This document is the operator's runbook;
the user-facing contract is in the Terms of Service.

**Disclaimer.** This document is engineering and operational
guidance, not legal advice. The procedure described here implements
the technical primitives Nova provides; whether following it
satisfies your legal obligations depends on your jurisdiction and
your counsel's advice. Names of statutes (DMCA, EUCD, etc.) are
referenced for context only.

## Overview: quarantine-first by default

v2 ships **quarantine-first** as the default takedown action. The
key is preserved under the operator's control during the counter-
notification window, public reads are blocked, and the bytes are
unrecoverable to viewers but reversible by the operator. After the
window expires with no counter-notice, a scheduled job tombstones
and crypto-shreds.

Operators who prefer the immediate-tombstone-and-shred flow (faster,
irreversible, more aggressive) can opt in via:

```yaml
moderation:
  takedown_default_action: tombstone   # default: 'quarantine'
```

Severe-content takedowns (CSAM-class) follow a separate procedure
and are NEVER auto-shredded. See
`docs/legal/SEVERE_CONTENT_PROCEDURE.md`.

## Lifecycle (quarantine-first default)

```
  Notice received
        │
        ▼
  Validation: meets statutory requirements?
        │
        ├── No  → Reject; record in dmca_cases with status='rejected'
        ▼
  Investigation: is the targeted content present? infringement plausible?
        │
        ├── No  → Close; record with status='rejected'
        ▼
  Action: state='quarantined', signed-URL revocation, schedule tombstone
        │   (key NOT yet shredded; legal_hold remains false)
        ▼
  Notify the uploader; advance their repeat-infringer counter
        │
        ▼
  Counter-notification window (10-14 business days, US DMCA)
        │
        ├── Counter received → forward to claimant; pause scheduled tombstone
        │     │
        │     ├── Claimant files suit within 10-14 days → keep quarantined
        │     ├── No suit → restore (state='active'), record outcome
        │     └── Operator restores manually after review
        ▼
  No counter received before scheduled_tombstone_at → tombstone + crypto-shred
        │
        ▼
  Close case; record with status='actioned'
        │
        ▼
  Retain dmca_cases row (NOT the bytes; the bytes are unrecoverable)
```

## Receipt

Notices arrive at one of three places:

1. **`POST /legal/dmca`** — the public HTTP endpoint defined in
   `openapi.yaml`. The submission must include the elements required
   by 17 U.S.C. § 512(c)(3): identification of the copyrighted work,
   identification of the allegedly infringing material (the CID),
   the claimant's contact information, a good-faith statement, an
   accuracy statement, and a physical or electronic signature.

2. **Email** to the designated agent. The operator's intake script
   (or a moderator) creates the equivalent `dmca_cases` row by
   hand or via `novactl dmca create`.

3. **Postal mail** to the designated agent. Same handling as email.

Every notice is recorded in `dmca_cases` regardless of validity.
This is by design: the audit trail of receipts is required by some
jurisdictions for safe-harbor accounting.

## Validation

A notice qualifies for processing when it meets all of:

- [ ] Identifies a specific copyrighted work (or a representative
      list, for multiple works).
- [ ] Identifies the allegedly infringing material with enough
      specificity to locate it. **The CID is the canonical
      identifier.** A vague description ("an image on your service")
      is rejected.
- [ ] Includes the claimant's name, physical address, telephone
      number, and email.
- [ ] Includes a statement that the claimant has a good-faith
      belief that the use is not authorized by the copyright owner,
      its agent, or the law.
- [ ] Includes a statement, under penalty of perjury, that the
      claimant is authorized to act on behalf of the owner of the
      exclusive right that is allegedly infringed.
- [ ] Is signed (physical or electronic signature).

If any element is missing, the moderator either:

- Replies to the claimant with the specific deficiency and a
  request to resubmit.
- Marks the case `dmca_status = 'rejected'` with a note describing
  the deficiency.

Rejected cases stay in the table for the operator's record-retention
period.

## Investigation

Once a notice qualifies, the moderator:

- [ ] Confirms the targeted CID exists in `blobs` and is in
      `state = 'active'`.
- [ ] Reviews the content if technically possible (it is — the
      moderator's role can decrypt via the read gateway).
- [ ] Forms a reasonable belief that the claim is plausible.
      Borderline cases (fair use, clear authorization, obvious
      misuse of the takedown system) are documented and either
      rejected or escalated to counsel.

Most notices are routine. Spending 15-20 minutes investigating each
is the right SLA target; faster than that risks bad-faith takedowns;
slower than that risks safe-harbor problems.

## Action (quarantine-first default)

To execute a quarantine-first takedown:

```sh
novactl moderation quarantine <cid> \
    --case <dmca_case_id> \
    --reason "DMCA notice from {{claimant}}" \
    --tombstone-after 14d
```

This command, in a single transaction:

1. INSERT `moderation_decisions(cid, rule='dmca', rule_ref=<case_id>,
   action='quarantine', scheduled_tombstone_at=now()+14d,
   legal_hold=false)`.
2. UPDATE `blobs` SET `state='quarantined'`. Cascade to all child
   derivatives via the product's `OnDelete` hook (which for
   quarantine cascades the state without shredding keys).
3. INSERT `signed_url_revocations (kind='cid', value=<cid>)` so
   outstanding signed URLs immediately fail verification.
4. INSERT audit log entries.
5. UPDATE `dmca_cases.status='actioned'`, `actioned_at=now()`.
6. Increment `takedown_repeat_infringers.strikes` for the uploader
   (created if not present).

The encryption key is **not** shredded. It remains in
`data_encryption_keys` with `state='active'`. The blob's bytes
remain on donor disks until either the scheduled tombstone fires
or the operator restores.

A scheduled job runs every minute and tombstones overdue rows:

```sql
SELECT md.cid
  FROM moderation_decisions md
  JOIN data_encryption_keys dek
    ON dek.id = (SELECT encryption_key_id FROM blobs WHERE cid = md.cid)
 WHERE md.scheduled_tombstone_at < now()
   AND md.scheduled_tombstone_at IS NOT NULL
   AND dek.legal_hold = false;
```

For each result, the job runs the tombstone procedure (state
transition, crypto-shred, federation unpin broadcast). Because
`legal_hold` is checked, severe-content rows whose `legal_hold=true`
will never be tombstoned by this job, even if a malformed schedule
were ever set.

## Action (immediate-tombstone, opt-in)

Operators who set `moderation.takedown_default_action: tombstone`
get an immediate-shred flow. The CLI command is the same:

```sh
novactl moderation takedown <cid> \
    --case <dmca_case_id> \
    --reason "DMCA notice from {{claimant}}"
```

The transaction:

1. INSERT `moderation_decisions(... action='tombstone', legal_hold=false)`.
2. UPDATE `blobs.state='tombstoned'`.
3. UPDATE `data_encryption_keys.state='shredded'`,
   `wrapped_key=zeroes(72)`, `shredded_at=now()`. Refused if
   `legal_hold=true` (which never holds for routine DMCA).
4. INSERT `signed_url_revocations (kind='cid', value=<cid>)`.
5. Cascade to all child derivatives (state, key shred).
6. INSERT pin-broadcast unpin entries for the federation.
7. UPDATE `dmca_cases.status='actioned'`.
8. Increment repeat-infringer strikes.
9. Audit-log every step.

This is irreversible. Counter-notification cannot restore the bytes;
the user must re-upload, which produces a different envelope CID
(fresh nonce). Operators choosing this mode trade reversibility
for simpler operational state.

The CLI prompts for confirmation before running the destructive
operations. `--no-confirm` skips the prompt for automation.

## Notification of the uploader

After action, notify the uploader:

- [ ] Compose a clear notice describing what was taken down (CID,
      filename if available), why (DMCA notice from `{claimant}`),
      whether the action is reversible (quarantine) or final
      (tombstone), and how to submit a counter-notification.
- [ ] Send via the user's registered email.
- [ ] Record the notification in the `audit_log`.

The notification is required by 17 U.S.C. § 512(g)(2)(A).

## Counter-notification

A user who believes the takedown was issued in error may submit a
counter-notification. Per § 512(g)(3), it must include:

- The user's name, address, telephone number.
- Identification of the material removed and its prior location.
- A statement under penalty of perjury that the user has a
  good-faith belief the material was removed by mistake or
  misidentification.
- Consent to jurisdiction in the federal court where the user
  resides (or the operator is located, for non-US users).
- The user's physical or electronic signature.

On receipt of a valid counter-notification:

- [ ] Forward to the claimant within the time required by your
      jurisdiction.
- [ ] **For quarantine-first actions:** clear
      `scheduled_tombstone_at` so the tombstone job will not fire.
      The blob remains quarantined until either the claimant files
      suit (extend the hold) or the operator decides to restore
      (UPDATE `blobs.state='active'`).
- [ ] **For immediate-tombstone actions:** the bytes are gone.
      Restoration requires the user to re-upload. Document this
      explicitly in your TOS.
- [ ] Record outcome in `dmca_cases.notes`.

The architectural choice in v2's quarantine-first default is to
preserve reversibility through the entire counter-notification
window. This trades operational complexity (extra state to manage)
for user-fairness and reduced legal exposure if takedowns turn out
to be mistaken.

## Repeat-infringer accounting

`takedown_repeat_infringers.strikes` is the running counter per
user. Once a user reaches the configured threshold (default 3
within the configured window), the moderator:

- [ ] Suspends the account.
- [ ] Records the termination decision in `audit_log`.
- [ ] Sends a final notice with the appeal channel from the TOS.

Termination is permanent absent successful appeal. The account's
content is not automatically deleted (it remains under the same
takedown rules); the user simply loses upload privileges.

## Record retention

| Record | Retention | Why |
|---|---|---|
| `dmca_cases` row | At least 3 years (US safe-harbor norms) | Required for repeat-infringer accounting and litigation discovery |
| `moderation_decisions` row | Same as `dmca_cases` | Forensic trail of the action |
| `audit_log` entries | Operator-defined; recommended ≥ 7 years | Litigation discovery |
| Encrypted bytes (quarantine) | Until `scheduled_tombstone_at` fires (default 14 days) | Reversibility window |
| Encrypted bytes (tombstone) | Until donor unpin propagation (≤ `evicted_after_seconds`) | After shred, bytes are unrecoverable; retention is moot |
| Plaintext content | **Never persisted post-decrypt** | Defense-in-depth |
| Notification emails | Operator-defined; recommended ≥ 1 year | Proof of compliance with § 512(g)(2)(A) |

We retain **case files**, not content. After a tombstone, the
`dmca_cases` row is sufficient to demonstrate the action; the bytes
are gone by design.

## Bad-faith notices

Repeated bad-faith DMCA notices from a single claimant may
themselves violate § 512(f). The operator may:

- [ ] Document the pattern in `dmca_cases.notes`.
- [ ] Decline to action future notices from the same claimant
      pending counsel review.
- [ ] In severe cases, refer to counsel for § 512(f) action.

This is a counter-pressure mechanism the architecture supports but
does not automate. The decision to push back on a claimant is a
business and legal one.

## Audit trail

Every step above writes to `audit_log` with:

- `actor_id` — the moderator's user UUID.
- `action` — `dmca.received`, `dmca.investigated`, `dmca.quarantined`,
  `dmca.tombstoned`, `dmca.rejected`, `dmca.counter_received`,
  `dmca.restored`, etc.
- `target_type` — `cid` or `dmca_case_id`.
- `target_id` — the actual CID or case UUID.
- `payload` — JSON with relevant context (claimant, notes, case id,
  scheduled_tombstone_at).

The audit log is append-only at the application layer; revoke
`UPDATE` and `DELETE` on `audit_log` for the application database
role in production.

## Cross-references

- Statutory mechanics: `TOS_TEMPLATE.md` § 6 ("DMCA / Takedown procedure")
- Crypto-shred mechanics: `docs/specs/ENCRYPTION_ENVELOPE.md` § "Crypto-shredding"
- Schema: `docs/specs/DATA_MODEL.sql` (`dmca_cases`,
  `moderation_decisions`, `data_encryption_keys`,
  `signed_url_revocations`)
- Pin propagation: `docs/specs/FEDERATION_PROTOCOL.md` § "Tombstone propagation"
- Signed-URL revocation: `docs/specs/SIGNED_URL_FORMAT.md` § "Revocation"
- Severe content (CSAM-class): `docs/legal/SEVERE_CONTENT_PROCEDURE.md`
- Operator launch readiness: `OPERATOR_CHECKLIST.md`
