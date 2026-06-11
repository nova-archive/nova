# Superpowers Design Specs — Index

This directory holds the **per-milestone design specs** produced under the
`superpowers:brainstorming` skill — the "what and why" for each milestone
(behavior, API shapes, state machines, exit criteria). They are organized **by
phase**.

> These milestone design specs are **distinct from the normative cross-phase
> contracts** in [`docs/specs/`](../../specs/) (FEDERATION_PROTOCOL,
> ENCRYPTION_ENVELOPE, HEALING_PROTOCOL, ARCHITECTURE_DECISIONS, DATA_MODEL, …).
> Those define Tier-1/Tier-2 protocol invariants that span phases and are
> **not** filed under `phase1/`/`phase2/`. The implementation plans that pair
> with these designs live in [`../plans/`](../plans/).

## Phase 1 — Single-node MVP (complete, `v0.1.0-rc1`)

Located in [`phase1/`](phase1/). Designs begin at M3; M1/M2 are covered in the
master design and the M2 plan.

| Doc | Milestone |
|---|---|
| [phase1/2026-05-25-phase1-single-node-mvp-design.md](phase1/2026-05-25-phase1-single-node-mvp-design.md) | **Master design** (architecture, container topology, M1–M14 breakdown) |
| [phase1/2026-05-28-phase1-m3-storage-read-api-design.md](phase1/2026-05-28-phase1-m3-storage-read-api-design.md) | M3 — storage read API |
| [phase1/2026-05-29-phase1-m4-upload-pipeline-design.md](phase1/2026-05-29-phase1-m4-upload-pipeline-design.md) | M4 — upload pipeline |
| [phase1/2026-05-29-phase1-m5-image-transforms-design.md](phase1/2026-05-29-phase1-m5-image-transforms-design.md) | M5 — image transforms |
| [phase1/2026-05-30-phase1-m6-auth-design.md](phase1/2026-05-30-phase1-m6-auth-design.md) | M6 — auth |
| [phase1/2026-05-31-m6.1-keystore-secret-mount-design.md](phase1/2026-05-31-m6.1-keystore-secret-mount-design.md) | M6.1 — keystore secret mount |
| [phase1/2026-06-01-phase1-m7-signed-urls-design.md](phase1/2026-06-01-phase1-m7-signed-urls-design.md) | M7 — signed URLs |
| [phase1/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md](phase1/2026-06-02-phase1-m8-integrity-audit-scheduler-design.md) | M8 — integrity audits |
| [phase1/2026-06-02-phase1-m9-moderation-design.md](phase1/2026-06-02-phase1-m9-moderation-design.md) | M9 — moderation |
| [phase1/2026-06-03-phase1-m10-master-key-rotation-design.md](phase1/2026-06-03-phase1-m10-master-key-rotation-design.md) | M10 — master-key rotation |
| [phase1/2026-06-04-phase1-m11-admin-spa-design.md](phase1/2026-06-04-phase1-m11-admin-spa-design.md) | M11 — admin SPA |
| [phase1/2026-06-07-phase1-m12-upload-widget-design.md](phase1/2026-06-07-phase1-m12-upload-widget-design.md) | M12 — upload widget |
| [phase1/2026-06-08-phase1-m13-setup-wizard-design.md](phase1/2026-06-08-phase1-m13-setup-wizard-design.md) | M13 — setup wizard |
| [phase1/2026-06-09-phase1-m14-polish-release-design.md](phase1/2026-06-09-phase1-m14-polish-release-design.md) | M14 — polish / release |

## Phase 2 — Federation + streaming-AEAD (in design)

Located in [`phase2/`](phase2/).

| Doc | Scope |
|---|---|
| [phase2/2026-06-11-phase2-federation-design.md](phase2/2026-06-11-phase2-federation-design.md) | **Master design** — donor nodes, binary split, mesh federation, healing, possession audits, streaming-AEAD; spec-reconciliation backlog |

Per-milestone Phase 2 designs (P2-M0 … P2-M10) are added to `phase2/` as each
milestone begins, mirroring the Phase 1 cadence.

## Tooling

- [`scripts/check_doc_links.py`](../../../scripts/check_doc_links.py) validates that local
  Markdown links resolve. Run `python3 scripts/check_doc_links.py docs` after any
  doc move. It is a **manual** check (not CI-wired): it intentionally reports the
  pre-existing un-captured `quickstart` screenshot placeholders, and a CI gate over
  prose containing code is prone to false positives.

> **Reorg note (2026-06-11):** Phase 1 milestone docs moved from this directory's
> top level into `phase1/`. All Markdown cross-references were updated. A few
> **immutable** code comments retain the pre-reorg paths by design — the frozen
> migrations under `internal/db/migrations/` (`migrations-frozen` gate) and the
> sqlc-generated files under `internal/db/gen/` (kept in lockstep with their
> unchanged query sources so `codegen-check` stays green). These are historical
> pointers and are not load-bearing.
