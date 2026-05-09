# Roadmap

## Phase 0 — Specifications (current)

Lock the protocol and contracts as documents before any production
code is written.

- [x] Repository skeleton + naming hygiene CI
- [ ] OpenAPI specification (`docs/specs/openapi.yaml`)
- [ ] Federation protocol (`docs/specs/FEDERATION_PROTOCOL.md`)
- [ ] Encryption envelope (`docs/specs/ENCRYPTION_ENVELOPE.md`)
- [ ] Data model (`docs/specs/DATA_MODEL.sql`)
- [ ] Signed URL format (`docs/specs/SIGNED_URL_FORMAT.md`)
- [ ] IPFS daemon hardening (`docs/specs/KUBO_HARDENING.md`)
- [ ] Product module interface (`docs/specs/PRODUCT_MODULE_INTERFACE.md`)
- [ ] Threat model (`docs/THREAT_MODEL.md`)
- [ ] Privacy audit (`docs/PRIVACY_AUDIT.md`)
- [ ] Operator checklist (`docs/legal/OPERATOR_CHECKLIST.md`)
- [ ] ToS template (`docs/legal/TOS_TEMPLATE.md`)
- [ ] Takedown procedure (`docs/legal/DMCA_PROCEDURE.md`)
- [ ] nginx reference config (`nginx/nova.conf.example`)
- [ ] Phase 0 dependency-only `docker-compose.yml`
- [ ] Cloudflare recipe (`docs/recipes/CLOUDFLARE.md`)

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
