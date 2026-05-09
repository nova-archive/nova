# Terms of Service Template

> **READ FIRST.** This is a starting-point template, not a finished
> Terms of Service. It is **not legal advice.** Before publishing a
> Terms of Service for your deployment, an attorney licensed in your
> jurisdiction must review and adapt this document for your specific
> service, user base, and applicable law.
>
> Placeholders in this document use the form `{{PLACEHOLDER}}`. Every
> placeholder must be replaced with a concrete value before the
> document is published. Several sections require choices that depend
> on your jurisdiction and your community's norms; those choices are
> annotated `[OPERATOR DECISION]` with brief context.

---

# Terms of Service for {{SERVICE_NAME}}

**Effective date:** {{EFFECTIVE_DATE}}

**Service operator:** {{OPERATOR_LEGAL_NAME}}, located at
{{OPERATOR_ADDRESS}}, contactable at {{OPERATOR_CONTACT_EMAIL}}.

## 1. Acceptance of these Terms

By accessing, registering for, or using {{SERVICE_NAME}} (the
"Service"), you agree to be bound by these Terms of Service (the
"Terms"). If you do not agree, you may not use the Service.

The Service is operated by {{OPERATOR_LEGAL_NAME}} ("we", "us", "our")
and is provided to you ("you", "user") subject to these Terms.

## 2. Description of the Service

The Service provides hosting and retrieval of digital content
(images, files, or other binary objects, depending on the Service's
configuration) for users and their communities. The Service operates
on a federated storage architecture in which encrypted copies of
user-uploaded content are distributed across volunteer-operated
storage nodes; only the Service's coordinator infrastructure holds
the keys necessary to decrypt content for delivery to authorized
viewers.

The Service is provided on an "as is" and "as available" basis. See
Sections 8 and 9 for warranty disclaimers and liability limitations.

## 3. User accounts and responsibilities

To upload content, you must register for an account. You agree:

- To provide accurate and current information at registration.
- To maintain the security of your account credentials.
- That you are responsible for all activity originating from your
  account.
- That you will notify us promptly of any unauthorized use.

You may close your account at any time. Closure does not waive
obligations you incurred before closure (e.g., for content you
uploaded that subsequently became the subject of a takedown).

## 4. Acceptable use

You may not, and may not encourage others to:

- Upload, share, or transmit content that infringes the intellectual
  property rights of others.
- Upload child sexual abuse material (CSAM) or non-consensual
  intimate imagery (NCII). The Service performs perceptual-hash
  scanning at upload against operator-configured blocklists; matches
  are rejected before any bytes are persisted.
- Upload content that violates applicable law in {{GOVERNING_JURISDICTION}}
  or in the jurisdiction where the content originates.
- Use the Service to harass, threaten, or impersonate any person.
- Attempt to circumvent the Service's authentication, rate limits,
  moderation, or technical protections.
- Use the Service in any way that materially interferes with its
  operation or the experience of other users.

[OPERATOR DECISION] Add or remove categories above to match the
norms and legal exposure of your community. For example, an academic
preprint host might explicitly allow scholarly fair-use quoting; a
fediverse instance might explicitly prohibit federated-spam content.

## 5. Content ownership and license

You retain all ownership rights in content you upload. By uploading
content, you grant us a non-exclusive, worldwide, royalty-free
license to:

- Store the content (in encrypted form, on infrastructure operated
  by us and our federated storage partners).
- Transmit and display the content as needed to deliver the Service
  to users and viewers you have authorized.
- Generate technical derivatives (resized images, format-converted
  copies, etc.) as required to render the content.

This license terminates when you delete the content from the Service.
The Service implements deletion as cryptographic erasure
("crypto-shredding") of the per-blob encryption key, which renders
the encrypted bytes mathematically unreadable even though they may
persist on volunteer storage disks for up to {{MAX_OFFLINE_WINDOW_DAYS}}
days while propagation completes.

## 6. DMCA / Takedown procedure

We respond to notices of claimed copyright infringement that comply
with the U.S. Digital Millennium Copyright Act (17 U.S.C. § 512) and
analogous statutes in other jurisdictions.

**Designated agent for receipt of DMCA notices:**
{{DMCA_AGENT_NAME}}, {{DMCA_AGENT_ADDRESS}}, {{DMCA_AGENT_EMAIL}}.

To submit a takedown notice, send a written communication to the
designated agent or submit a notice at `/legal/dmca` containing the
elements required by 17 U.S.C. § 512(c)(3): identification of the
copyrighted work, identification of the allegedly infringing
material with sufficient detail to locate it (the content's CID is
ideal), your contact information, a good-faith statement, an
accuracy statement, and your physical or electronic signature.

We may, at our discretion and in compliance with the safe-harbor
provisions, action a takedown by tombstoning the content (which
crypto-shreds the encryption key and broadcasts an unpin command
across the federation).

If you believe a takedown was issued in error, you may submit a
counter-notification to the same agent that complies with 17 U.S.C.
§ 512(g)(3). We follow the procedures in § 512(g) for restoration
and notice.

## 7. Repeat-infringer policy

We terminate accounts of users who repeatedly infringe the
intellectual property rights of others. The threshold is
{{REPEAT_INFRINGER_STRIKES}} valid takedowns within
{{REPEAT_INFRINGER_WINDOW}}. Account termination is permanent;
appeals must be made in writing to {{APPEALS_CONTACT}}.

## 8. Disclaimers

THE SERVICE IS PROVIDED "AS IS" AND "AS AVAILABLE" WITHOUT WARRANTY
OF ANY KIND. WE DISCLAIM ALL WARRANTIES, EXPRESS OR IMPLIED,
INCLUDING WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR
PURPOSE, NON-INFRINGEMENT, AND ANY WARRANTY ARISING FROM COURSE OF
DEALING.

We do not warrant that the Service will be uninterrupted,
error-free, or secure, or that any defects will be corrected. You
acknowledge that distributed storage carries operational risks
including but not limited to: temporary loss of donor capacity, the
need to re-replicate content across the federation, and the
possibility of permanent content loss in events that exceed the
configured replication factor.

## 9. Limitation of liability

TO THE MAXIMUM EXTENT PERMITTED BY APPLICABLE LAW, IN NO EVENT WILL
{{OPERATOR_LEGAL_NAME}} BE LIABLE FOR ANY INDIRECT, INCIDENTAL,
SPECIAL, CONSEQUENTIAL, OR PUNITIVE DAMAGES, INCLUDING WITHOUT
LIMITATION DAMAGES FOR LOST PROFITS, LOST DATA, OR OTHER INTANGIBLE
LOSSES, ARISING FROM OR RELATED TO YOUR USE OF OR INABILITY TO USE
THE SERVICE.

OUR AGGREGATE LIABILITY FOR ALL CLAIMS RELATED TO THE SERVICE WILL
NOT EXCEED {{LIABILITY_CAP_AMOUNT}} OR THE AMOUNT YOU PAID US IN THE
TWELVE MONTHS PRECEDING THE EVENT GIVING RISE TO THE CLAIM,
WHICHEVER IS GREATER.

[OPERATOR DECISION] If your service is free, the
"or amount paid" clause defaults to zero; the
`{{LIABILITY_CAP_AMOUNT}}` becomes the only cap. Counsel will advise
on a defensible figure.

## 10. Indemnification

You agree to indemnify and hold harmless {{OPERATOR_LEGAL_NAME}},
its officers, employees, and donor storage partners from any claim
or demand arising from content you upload or your use of the Service
in breach of these Terms.

We reserve the right to assume the exclusive defense of any matter
otherwise subject to indemnification by you, in which case you will
cooperate with our defense.

## 11. Termination

We may suspend or terminate your account at our discretion, with or
without notice, for breach of these Terms. On termination:

- You lose access to your account and to content uploaded under it.
- Content marked for deletion is crypto-shredded as described in
  Section 5.
- Sections 5 (license; survival of grants for already-distributed
  derivatives), 8 (disclaimers), 9 (liability), 10 (indemnification),
  and 13 (governing law) survive.

## 12. Privacy

Our Privacy Policy ({{PRIVACY_POLICY_URL}}) describes the data we
collect, how it is processed, retention periods, and how you may
exercise your rights under applicable privacy law.

Highlights:

- Content is stored encrypted; donor storage nodes do not have
  access to plaintext.
- We retain source IP addresses for {{SOURCE_IP_RETENTION_DAYS}}
  days for moderation and security.
- We do not sell user data to third parties.
- We do not run third-party analytics; the Service is hermetic with
  respect to external assets.

To exercise data-subject rights (access, deletion, correction),
contact {{DSAR_CONTACT}}.

## 13. Governing law and dispute resolution

These Terms are governed by the laws of {{GOVERNING_JURISDICTION}},
without regard to its conflict-of-laws provisions. Any dispute
arising from these Terms or from your use of the Service will be
resolved exclusively in the courts of {{VENUE}}.

[OPERATOR DECISION] Choose between mandatory arbitration, court
litigation, or a hybrid. Counsel will advise on the appropriate
mechanism for your jurisdiction and user base.

## 14. Changes to these Terms

We may modify these Terms at any time. Material changes will be
announced at {{ANNOUNCEMENT_CHANNEL}} at least
{{NOTICE_PERIOD_DAYS}} days before they take effect. Continued use
of the Service after the effective date constitutes acceptance of
the modified Terms.

## 15. Severability

If any provision of these Terms is held invalid or unenforceable,
the remaining provisions remain in full force.

## 16. Entire agreement

These Terms, together with the Privacy Policy referenced in
Section 12, constitute the entire agreement between you and
{{OPERATOR_LEGAL_NAME}} regarding the Service.

## 17. Contact

Questions about these Terms: {{OPERATOR_CONTACT_EMAIL}}.
DMCA notices: {{DMCA_AGENT_EMAIL}}.
DSAR / privacy: {{DSAR_CONTACT}}.

---

## Placeholder reference

| Placeholder | Description |
|---|---|
| `{{SERVICE_NAME}}` | The customer-facing name of your deployment |
| `{{EFFECTIVE_DATE}}` | ISO date these Terms take effect |
| `{{OPERATOR_LEGAL_NAME}}` | The legal entity operating the Service |
| `{{OPERATOR_ADDRESS}}` | Registered address |
| `{{OPERATOR_CONTACT_EMAIL}}` | General-contact email |
| `{{GOVERNING_JURISDICTION}}` | E.g., "the State of California, United States" |
| `{{VENUE}}` | E.g., "Santa Clara County, California" |
| `{{MAX_OFFLINE_WINDOW_DAYS}}` | The federation's `max_offline_window_seconds` setting, expressed in days (default 30) |
| `{{DMCA_AGENT_NAME}}` | Designated agent (must match U.S. Copyright Office filing) |
| `{{DMCA_AGENT_ADDRESS}}` | Designated agent address |
| `{{DMCA_AGENT_EMAIL}}` | Designated agent email |
| `{{REPEAT_INFRINGER_STRIKES}}` | Number of strikes before termination (default 3) |
| `{{REPEAT_INFRINGER_WINDOW}}` | Sliding window (e.g., "12 months") |
| `{{APPEALS_CONTACT}}` | Email or address for termination appeals |
| `{{LIABILITY_CAP_AMOUNT}}` | E.g., "USD 100" |
| `{{PRIVACY_POLICY_URL}}` | Stable URL of your Privacy Policy |
| `{{SOURCE_IP_RETENTION_DAYS}}` | Match `operator.yaml: source_ip_retention_days` (default 30; 1 in `paranoid: true`) |
| `{{DSAR_CONTACT}}` | Email or address for data-subject requests |
| `{{ANNOUNCEMENT_CHANNEL}}` | Where Terms changes are announced (e.g., "the Service homepage and our official mailing list") |
| `{{NOTICE_PERIOD_DAYS}}` | Material-change notice period (commonly 14 or 30) |
