# Recipe: Automated donor onboarding

Status: **Operator-side pattern.** Nova does not ship this. The
protocol provides cryptographic primitives (cert issuance,
revocation, reputation, possession audits); this document describes
how an operator can build a self-serve onboarding portal on top of
them.

## What this is for

The default onboarding path in `VOLUNTEER_DEPLOYMENT_GUIDANCE.md`
is out-of-band: the operator manually emails a candidate donor a
bundle containing a Nebula cert, federation client cert,
`swarm.key`, lighthouse address, and coordinator URL. This works
for small federations where the operator personally vets each
donor. It does not scale to federations that expect to grow into
the dozens or hundreds of nodes, or to communities that prefer
self-serve enrollment.

This recipe describes a pattern an operator can deploy outside of
Nova to issue donor credentials automatically, with safeguards to
prevent the automated pipeline from becoming a Sybil-injection
vector.

## What changes versus the default

The protocol does not change. The operator still runs a single CA
that the coordinator and donor nodes trust. The credentials a
donor receives are exactly the same bundle the manual flow
distributes. What changes is the **issuance path**: a public web
portal, with safeguards, replaces the manual email.

## Architecture

Three new components, all operator-deployed and outside the
Nova binaries:

1. **Sub-CA for automated issuance.** The root CA signs an
   intermediate CA whose only purpose is signing
   automatically-issued federation client certs and Nebula certs.
   The root CA's private key never touches the public portal.
2. **Public-facing gatekeeper API.** A small web service (operator's
   choice of language) sitting outside the Nebula mesh, behind the
   operator's TLS termination. Holds the sub-CA's signing key.
   Receives onboarding requests; applies friction (see below);
   signs and returns the bundle.
3. **Quarantine overlay.** A second Nebula subnet, segregated from
   production. Newly-onboarded donors join this subnet first.
   Their certs carry a group tag that the coordinator's federation
   endpoint recognizes as probationary. Production CIDs are not
   assigned to probationary donors.

```
              Public Internet
                    │
                    ▼
        +-------------------------+
        |  Public Gatekeeper API  |  ← OIDC / PoW / invite tokens
        |  (sub-CA signing key)   |
        +-----------+-------------+
                    │
                    ▼
        Issues prospect.crt + quarantine_swarm.key
                    │
                    ▼
              Donor joins quarantine subnet
                    │
                    ▼
        Synthetic audit harness (capacity probe)
                    │
                    ▼
                 Pass?
                /     \
              Yes      No → revoke prospect.crt
               │
               ▼
        Promote: production donor.crt + production swarm.key
        (root-CA-signed)
```

## Friction layer

The manual onboarding path uses human judgment as its friction.
The automated path must use machine-checkable friction; otherwise
the system gladly issues credentials to a botnet.

Operator picks one or more of:

- **Invite tokens.** A trusted user generates a single-use token
  via an operator-side admin command. The portal accepts the
  token, issues the bundle, and revokes the token. This is the
  closest analog to the manual flow.
- **OIDC binding.** The portal requires the requester to
  authenticate via the operator's chosen identity provider
  (GitHub, an existing community SSO, etc.), with operator-set
  account-age and activity thresholds.
- **Proof-of-work.** The portal issues a hashcash challenge before
  signing. Tuned to a few seconds of consumer CPU; tuned harder if
  the operator observes abuse.
- **Operator review queue.** The portal collects requests and
  queues them for asynchronous human approval. This is invite
  tokens by another name and is functionally identical to the
  default flow; included for completeness.

These are independently composable. Strict deployments combine all
of them; lenient ones use one.

## Quarantine and promotion

A donor that completes the friction layer receives credentials
that join a **quarantine** Nebula subnet, not production. The
coordinator's federation endpoint accepts mTLS handshakes from
quarantine certs but applies two restrictions:

1. The donor is not assigned any production CIDs. The
   orchestrator filters quarantine donors out of placement and
   healing candidates.
2. The audit subsystem subjects the donor to synthetic possession
   audits against pre-staged dummy payloads. The synthetic payloads
   are designed to test capacity claims: a donor that registered
   with `bandwidth_budget_bytes_per_day = 100 GB` is offered
   dummy CIDs that exercise that budget over a configurable
   window.

After the donor passes the synthetic audit window (operator's
choice of duration; 24-72 hours is typical), the promotion
service:

- Signs a production federation client cert using the root CA.
- Distributes the production `swarm.key`.
- Asks the donor's node to re-register with the production
  credentials.
- Revokes the prospect cert.

The donor is now indistinguishable from a manually-onboarded one.

## What Nova provides

- **Cert issuance.** The operator's CA can be operated programmatically.
- **Cert revocation.** `novactl node revoke` works on either prospect or production certs.
- **Reputation tracking.** New nodes start at the configured starting reputation; failed audits decrement.
- **Possession audits.** The same challenge-response protocol used in production exercises the synthetic payloads.
- **`/fed/v1/register` idempotency.** A donor switching from prospect to production credentials re-registers cleanly.
- **Group tags in Nebula certs.** Nebula's cert format permits group tags the coordinator uses to identify quarantine donors.

## What the operator must build

- The public gatekeeper API itself.
- The sub-CA infrastructure (offline root CA, online sub-CA, revocation propagation).
- The quarantine Nebula subnet and its routing.
- The synthetic-audit harness (pre-staged dummy payloads, schedule, promotion criteria).
- The promotion service (calls the operator's existing CA tools to issue production credentials and Nova's revocation API to retire prospect credentials).
- Monitoring for the gatekeeper API itself (it becomes an attack surface the manual flow did not have).

## What to watch for

- **Sub-CA compromise is bounded.** If the gatekeeper API is compromised, the attacker can mint prospect certs but not production ones. The blast radius is the quarantine subnet; revoke the sub-CA and re-issue without touching the root.
- **Cost-tuning the friction.** The PoW or identity threshold has to be inconvenient enough to deter Sybil farming but light enough that legitimate volunteers do not bounce. Operators should monitor abandonment rates and tune accordingly.
- **Quarantine duration vs. promotion eagerness.** Too short and you promote nodes whose bandwidth claims are inflated; too long and you stall growth. 24 hours is the practical floor; 72 hours catches most misconfiguration.
- **Donor's hardware can pass synthetic audits and still fail production.** Synthetic audits measure capacity; they do not predict ISP intervention or hardware longevity. Reputation in production remains the primary signal; the quarantine is a first filter, not a guarantee.

## Cross-references

- `docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md` — the default manual flow
- `docs/specs/FEDERATION_PROTOCOL.md` — `/fed/v1/register` and revocation semantics
- `docs/specs/POSSESSION_AUDIT.md` — the audit protocol the quarantine uses
- `docs/specs/HEALING_PROTOCOL.md` — reputation feedback into placement
- `docs/specs/ARCHITECTURE_DECISIONS.md` § "Tier 3" — donor lifecycle and onboarding is operator's freedom
