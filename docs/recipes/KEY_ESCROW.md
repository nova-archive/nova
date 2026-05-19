# Recipe: Master-key backup escrow

Status: **Operator-side pattern.** Nova does not ship this. The
running coordinator continues to load the unwrapped master key
from `NOVA_MASTER_KEY`; the escrow pattern only applies to backups
and recovery, not the read path.

## What this is for

`THREAT_MODEL.md` § "Acknowledged residual risks" item 2:
master-key loss is permanent loss of every blob. Off-box, off-cloud
backups are the mitigation, but those backups are themselves
single points of failure: a single offline copy in a safe-deposit
box is one fire away from oblivion, and multiple copies multiply
the risk of one of them being compromised.

This recipe distributes the **backup** of `NOVA_MASTER_KEY` across
an N-of-M recovery committee using Shamir's Secret Sharing, so:

- Loss of any M−N shares is recoverable (the threshold property).
- Compromise of any N−1 shares reveals nothing about the secret.

This does not change Nova's trust model. The running coordinator
still holds the unwrapped master key; the operator still has
unilateral authority over the federation. The committee is
involved only in the recovery procedure.

## What changes versus the default

The default is: the operator writes `NOVA_MASTER_KEY` to a single
secure off-box location and trusts that location. The escrow
pattern replaces "one location" with "N-of-M committee members,"
preserving the operator-sovereignty model for the running system
while making catastrophic backup loss recoverable from a partial
committee turnout.

## Architecture

```
    Master key (256 bits) → Shamir(N, M) split
                            │
                ┌───────────┼───────────┐
                ▼           ▼           ▼
            Share 1     Share 2     Share M
              │           │             │
              ▼           ▼             ▼
        Committee   Committee     Committee
        member 1    member 2      member M
        (offline)   (offline)     (offline)
```

When the operator needs to recover the master key (host destroyed,
keys lost, suspected compromise requiring rotation to a new key):

```
    N of M committee members each retrieve their share
                        │
                        ▼
                Operator reconstructs MK = Shamir.reconstruct(shares)
                        │
                        ▼
                Set NOVA_MASTER_KEY=MK on the new host
                        │
                        ▼
                Resume normal operation
```

Recommended parameters:

- `M` ≥ 5 (committee size; allows for member loss without paralyzing recovery)
- `N` ≥ ⌈(M+1)/2⌉ (threshold; high enough to resist a quorum of compromised members)

A 3-of-5 scheme is the recommended starting point. A 5-of-9 scheme is appropriate for larger or more security-conscious deployments.

## What Nova provides

- `NOVA_MASTER_KEY` is loaded from the environment at coordinator boot.
- Multiple versions can be loaded simultaneously during rotation (`NOVA_MASTER_KEY_V1`, `NOVA_MASTER_KEY_V2`, `NOVA_MASTER_KEY_ACTIVE`). See `ENCRYPTION_ENVELOPE.md` § "Master key versioning".
- `novactl keys rotate-master` re-wraps every active per-blob key with a new master, supporting the case where the escrowed-and-reconstructed key is being rotated out of paranoia after recovery.

## What the operator must build

- The Shamir split-and-distribute tooling. Any reputable Shamir library suffices; the secret being split is 32 bytes of hex.
- The committee selection and ceremony. Who holds shares, how shares are physically stored, how members are reached when recovery is needed.
- A documented runbook that describes how to convene the committee, reconstruct the key, set up a new coordinator host, and rotate after recovery.
- A periodic rehearsal. Backups that have never been restored are folklore, not backups. The committee should rehearse reconstruction against a sacrificial test secret at least annually.

## What this does not protect against

- **Compromise of the running coordinator.** The escrow concerns the *backup* secret only. While Nova is running, the unwrapped master key sits in the coordinator's process memory; a coordinator compromise (Threat Model § B) bypasses the escrow entirely.
- **Coordinated committee compromise.** If N members collude or are simultaneously coerced, they can reconstruct the secret. Operators selecting committee members should consider jurisdictional and institutional diversity, the same way `OPERATOR_CHECKLIST.md` recommends diversifying donor hosting providers.
- **Loss of more than M−N shares.** The threshold is the threshold; lose enough shares and the secret is gone. The right response is to size M and N generously.

## What to watch for

- **Member turnover.** Committee members leave. A new share generated for a replacement member requires re-splitting and re-distributing to *every* member, because Shamir does not natively support adding shares to an existing split. Plan the ceremony cadence.
- **Share storage hygiene.** A share on a USB drive in a desk drawer is not stored. A share in a sealed envelope in a fireproof safe with a labeled retrieval procedure is stored.
- **The runbook is the part that fails.** Operators frequently get the cryptography right and the operational procedure wrong. Rehearse.

## Cross-references

- `docs/specs/ENCRYPTION_ENVELOPE.md` § "Master key versioning"
- `docs/THREAT_MODEL.md` § "Acknowledged residual risks" item 2
- `docs/THREAT_MODEL.md` § "Trust-model choices not implemented" — Threshold cryptography
- `docs/legal/OPERATOR_CHECKLIST.md` — what to back up and how often
- `docs/specs/ARCHITECTURE_DECISIONS.md` § "Tier 3" — backup strategy is operator's freedom
