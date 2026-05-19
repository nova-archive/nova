# Specifications

This directory contains the load-bearing specifications for Nova.
Production code in `cmd/`, `internal/`, `pkg/`, and `web/` implements
these specs faithfully. Drift between code and spec fails CI.

## Index

### Decision boundary

- [`ARCHITECTURE_DECISIONS.md`](ARCHITECTURE_DECISIONS.md) — three-tier
  classification of every architectural decision (protocol-enforced /
  operator-tunable / operator freedom). Start here to understand what
  may and may not be changed in a deployment.

### Wire formats and contracts

- [`openapi.yaml`](openapi.yaml) — complete HTTP API; source of truth for codegen
- [`FEDERATION_PROTOCOL.md`](FEDERATION_PROTOCOL.md) — node lifecycle, message schemas, repair transport
- [`ENCRYPTION_ENVELOPE.md`](ENCRYPTION_ENVELOPE.md) — per-blob encryption wire format, master-key versioning, crypto-shred
- [`SIGNED_URL_FORMAT.md`](SIGNED_URL_FORMAT.md) — HMAC canonicalization, structured revocation
- [`IPFS_IMPORT_RULES.md`](IPFS_IMPORT_RULES.md) — deterministic CID parameters

### Schema and storage

- [`DATA_MODEL.sql`](DATA_MODEL.sql) — Postgres DDL
- [`KUBO_HARDENING.md`](KUBO_HARDENING.md) — required IPFS daemon configuration; refuse-to-start validator

### Behavior

- [`HEALING_PROTOCOL.md`](HEALING_PROTOCOL.md) — orchestrator math, mass-casualty detection, slow-attrition detection
- [`PRODUCT_MODULE_INTERFACE.md`](PRODUCT_MODULE_INTERFACE.md) — how product layers plug into storage; upload pipeline contract
- [`INTEGRITY_AUDIT.md`](INTEGRITY_AUDIT.md) — Phase 1 local fixity checks
- [`POSSESSION_AUDIT.md`](POSSESSION_AUDIT.md) — Phase 2 donor challenge-response spot-checks

Specifications are normative. If a spec and the code disagree, the
spec is correct and the code must be updated.
