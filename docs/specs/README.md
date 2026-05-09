# Specifications

This directory contains the load-bearing specifications for Nova.
Production code in `cmd/`, `internal/`, `pkg/`, and `web/` implements
these specs faithfully. Drift between code and spec fails CI.

Planned files (Phase 0):

- `openapi.yaml` — complete HTTP API; source of truth for codegen
- `FEDERATION_PROTOCOL.md` — node lifecycle and message schemas
- `ENCRYPTION_ENVELOPE.md` — per-blob encryption wire format
- `DATA_MODEL.sql` — Postgres DDL
- `SIGNED_URL_FORMAT.md` — HMAC canonicalization
- `KUBO_HARDENING.md` — required IPFS daemon configuration
- `PRODUCT_MODULE_INTERFACE.md` — how product layers plug into storage

Specifications are normative. If a spec and the code disagree, the
spec is correct and the code must be updated.
