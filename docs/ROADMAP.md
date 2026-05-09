# Roadmap

## Phase 0 — Specifications (complete)

Lock the protocol and contracts as documents before any production
code is written.

- [x] Repository skeleton
- [x] OpenAPI specification (`docs/specs/openapi.yaml`)
- [x] Signed URL format (`docs/specs/SIGNED_URL_FORMAT.md`)
- [x] Data model (`docs/specs/DATA_MODEL.sql`)
- [x] Encryption envelope (`docs/specs/ENCRYPTION_ENVELOPE.md`)
- [x] Federation protocol (`docs/specs/FEDERATION_PROTOCOL.md`)
- [x] IPFS daemon hardening (`docs/specs/KUBO_HARDENING.md`)
- [x] Product module interface (`docs/specs/PRODUCT_MODULE_INTERFACE.md`)
- [x] Healing protocol (`docs/specs/HEALING_PROTOCOL.md`)
- [x] Orchestrator resilience simulation (`simulations/orchestrator_resilience.py`)
- [x] Threat model (`docs/THREAT_MODEL.md`)
- [x] Privacy audit (`docs/PRIVACY_AUDIT.md`)
- [x] Operator checklist (`docs/legal/OPERATOR_CHECKLIST.md`)
- [x] ToS template (`docs/legal/TOS_TEMPLATE.md`)
- [x] Takedown procedure (`docs/legal/DMCA_PROCEDURE.md`)
- [x] Volunteer deployment guidance (`docs/VOLUNTEER_DEPLOYMENT_GUIDANCE.md`)
- [x] nginx reference config (`nginx/nova.conf.example`)
- [x] nginx walkthrough (`docs/recipes/NGINX_REFERENCE.md`)
- [x] Phase 0 dependency-only `docker-compose.yml`
- [x] Cloudflare recipe (`docs/recipes/CLOUDFLARE.md`)

## Phase 1 — Single-node MVP

Standalone coordinator with embedded hardened IPFS daemon, Postgres,
nginx + certbot, signed-URL HMAC, per-blob encryption on by default,
on-the-fly image transforms, drag-and-drop upload widget. Exports
`pkg/coordinator` and `pkg/node` as semver-stable Go library packages.

## Phase 2 — Federation

Split coordinator from pinning-node binary. Mesh-VPN-authenticated
federation, replication-factor enforcement, donor-operated nodes.

## Phase 3 — Dedup and moderation

Perceptual hash index, near-duplicate detection, content-moderation
pipeline.

## Phase 4 — Adapters and SDKs

Adapter packages for fediverse and forum software (separate
repositories). Auto-generated client SDKs in TypeScript, Python, Swift.

## Phase 5 — Hardening

Chaos testing, security audit, documentation polish, public 1.0.

## Phase 6+ — Research

Speculative directions: end-user client direct integration, browser-
resident pinning via WASM, FFI bindings for non-Go embedding,
additional product modules (`nova-video`, `nova-audio`, `nova-archive`,
`nova-document`).
