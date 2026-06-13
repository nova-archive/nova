# Superpowers Implementation Plans — Index

This directory holds the **per-milestone implementation plans** produced under
the `superpowers:writing-plans` skill — the "how" for each milestone (file
structure, commit-sequenced tasks with `- [ ]` checkboxes, testing, gotchas).
They are organized **by phase** and pair with the designs in
[`../specs/`](../specs/).

> Agentic workers: each plan names its REQUIRED SUB-SKILL
> (`superpowers:subagent-driven-development` or `superpowers:executing-plans`)
> in its preamble. Execute task-by-task; the checkboxes track progress.

## Phase 1 — Single-node MVP (complete, `v0.1.0-rc1`)

Located in [`phase1/`](phase1/). M1 is detailed inside the master plan (§ M1);
every other milestone has its own plan.

| Plan | Milestone |
|---|---|
| [phase1/2026-05-25-phase1-single-node-mvp.md](phase1/2026-05-25-phase1-single-node-mvp.md) | **Master plan** (milestone table + M1 detailed) |
| [phase1/2026-05-25-phase1-m2-envelope-ipfs.md](phase1/2026-05-25-phase1-m2-envelope-ipfs.md) | M2 — envelope + IPFS round-trip |
| [phase1/2026-05-28-phase1-m3-storage-read-api.md](phase1/2026-05-28-phase1-m3-storage-read-api.md) | M3 — storage read API |
| [phase1/2026-05-29-phase1-m4-upload-pipeline.md](phase1/2026-05-29-phase1-m4-upload-pipeline.md) | M4 — upload pipeline |
| [phase1/2026-05-29-phase1-m5-image-transforms.md](phase1/2026-05-29-phase1-m5-image-transforms.md) | M5 — image transforms |
| [phase1/2026-05-30-phase1-m6-auth.md](phase1/2026-05-30-phase1-m6-auth.md) | M6 — auth |
| [phase1/2026-06-01-phase1-m7-signed-urls.md](phase1/2026-06-01-phase1-m7-signed-urls.md) | M7 — signed URLs |
| [phase1/2026-06-02-phase1-m8-integrity-audit-scheduler.md](phase1/2026-06-02-phase1-m8-integrity-audit-scheduler.md) | M8 — integrity audits |
| [phase1/2026-06-02-phase1-m9-moderation.md](phase1/2026-06-02-phase1-m9-moderation.md) | M9 — moderation |
| [phase1/2026-06-03-phase1-m10-master-key-rotation.md](phase1/2026-06-03-phase1-m10-master-key-rotation.md) | M10 — master-key rotation |
| [phase1/2026-06-04-phase1-m11-admin-spa.md](phase1/2026-06-04-phase1-m11-admin-spa.md) | M11 — admin SPA |
| [phase1/2026-06-07-phase1-m12-upload-widget.md](phase1/2026-06-07-phase1-m12-upload-widget.md) | M12 — upload widget |
| [phase1/2026-06-08-phase1-m13-setup-wizard.md](phase1/2026-06-08-phase1-m13-setup-wizard.md) | M13 — setup wizard |
| [phase1/2026-06-09-phase1-m14-polish-release.md](phase1/2026-06-09-phase1-m14-polish-release.md) | M14 — polish / release |

## Phase 2 — Federation + streaming-AEAD (in planning)

Located in [`phase2/`](phase2/).

| Plan | Scope |
|---|---|
| [phase2/2026-06-11-phase2-federation.md](phase2/2026-06-11-phase2-federation.md) | **Master plan** — milestone table P2-M0 … P2-M10; P2-M0 (spec reconciliation) and P2-M1 (repo separation) detailed |
| [phase2/2026-06-13-phase2-m0-spec-reconciliation.md](phase2/2026-06-13-phase2-m0-spec-reconciliation.md) | P2-M0 — spec reconciliation (docs-only gate; detailed authority) |

Each subsequent Phase 2 milestone gets its own detailed plan in `phase2/` at the
start of that milestone, the same cadence Phase 1 used.

> Phase 6 (post-1.0 multi-coordinator HA) is **design-only** so far — see the
> specs index `phase6/` entry; no implementation plan yet.
