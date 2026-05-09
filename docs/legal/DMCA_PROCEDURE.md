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

## Lifecycle overview

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
  Action: tombstone the CID, crypto-shred the key, broadcast unpin
        │
        ▼
  Notify the uploader; advance their repeat-infringer counter
        │
        ▼
  Counter-notification window (10 days, US DMCA)
        │
        ├── Counter received → Forward to claimant; restore content if no
        │                       suit filed within 10–14 business days
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

Most notices are routine. Spending 20 minutes investigating each is
the right SLA target; faster than that risks bad-faith takedowns;
slower than that risks safe-harbor problems.

## Action

To execute a takedown:

```sh
novactl moderation takedown <cid> \
    --case <dmca_case_id> \
    --reason "DMCA notice from {{claimant}}"
```

This command:

1. Inserts a row in `moderation_decisions` with
   `rule = 'dmca'`, `rule_ref = <case_id>`, `action = 'tombstone'`.
2. Updates `blobs.state` to `tombstoned`.
3. Crypto-shreds the per-blob key:
   `UPDATE keys SET state='shredded', shredded_at=now(),
    wrapped_key=zeroes WHERE id = <key_id>`.
4. Removes corresponding rows from `pin_assignments`. The donors'
   next poll of `/fed/v1/pins/assigned` will not include the CID;
   donors then `ipfs pin rm` and IPFS GC reclaims storage.
5. Inserts a `signed_url_revocations` row with prefix `cid:<cid>`
   so any outstanding signed URLs are immediately invalid.
6. Updates `dmca_cases.actioned_at = now()`,
   `status = 'actioned'`.
7. Increments the uploader's `takedown_repeat_infringers.strikes`
   row (created if not present).
8. Writes an `audit_log` entry naming the moderator and the case.

Equivalently, a moderator may execute the takedown through the
admin UI; the underlying calls are the same.

The action is deliberate and ordered: the audit log is written
**after** the destructive operations, so a partial failure leaves
investigatory traces. The crypto-shred is the point of no return —
once the key is zeroed, the bytes are unrecoverable, and counter-
notification cannot restore them. Operators must therefore verify
the action is correct **before** running `novactl moderation
takedown`. The CLI prompts for confirmation.

## Notification of the uploader

After action, notify the uploader:

- [ ] Compose a clear notice describing what was taken down (CID,
      filename if available), why (DMCA notice from {claimant}),
      and how to submit a counter-notification.
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
- [ ] If the claimant does not file suit within 10–14 business
      days, restore the content **only if** the bytes are still
      retrievable. Crypto-shred is final — restoration requires
      either re-uploading or, if the user kept a copy, re-uploading
      to the same CID (the underlying bytes will be different
      because the new envelope has a fresh nonce; functionally
      it's a fresh blob).
- [ ] Record outcome in `dmca_cases.notes`.

The architectural choice to crypto-shred immediately on action
trades reversibility for speed and certainty. Document this in
your TOS so users understand a counter-notified restoration may
require re-uploading.

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
| Encrypted bytes | Until tombstone-shred + donor unpin propagation (≤ `max_offline_window`, default 30 days) | After shred, bytes are unrecoverable; retention is moot |
| Plaintext content | **Never persisted post-decrypt** | Defense-in-depth |
| Notification emails | Operator-defined; recommended ≥ 1 year | Proof of compliance with § 512(g)(2)(A) |

We retain **case files**, not content. After a takedown, the
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
- `action` — `dmca.received`, `dmca.investigated`, `dmca.actioned`,
  `dmca.rejected`, `dmca.counter_received`, `dmca.restored`, etc.
- `target_type` — `cid` or `dmca_case_id`.
- `target_id` — the actual CID or case UUID.
- `payload` — JSON with relevant context (claimant, notes, case id).

The audit log is append-only at the application layer; revoke
`UPDATE` and `DELETE` on `audit_log` for the application database
role in production.

## Cross-references

- Statutory mechanics: `TOS_TEMPLATE.md` § 6 ("DMCA / Takedown procedure")
- Crypto-shred mechanics: `docs/specs/ENCRYPTION_ENVELOPE.md` § "Crypto-shredding"
- Pin propagation: `docs/specs/FEDERATION_PROTOCOL.md` § "Sequence: Tombstone propagation"
- Signed-URL revocation: `docs/specs/SIGNED_URL_FORMAT.md` § "Revocation"
- Operator launch readiness: `OPERATOR_CHECKLIST.md`
