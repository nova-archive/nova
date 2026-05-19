# Deployment Recipes

Annotated deployment recipes for common operator scenarios. Each
recipe is **operator-side**: it builds on Nova's existing
primitives rather than changing the protocol. The Nova binaries do
not ship the orchestration these recipes describe.

## CDN and routing

- `CLOUDFLARE.md` — fronting the Read Gateway with Cloudflare to
  collapse egress costs
- `NGINX_REFERENCE.md` — annotated reference site configuration

## Operations

- `AUTOMATED_ONBOARDING.md` — self-serve donor enrollment with
  sub-CA, friction layer, and quarantine subnet
- `KEY_ESCROW.md` — Shamir-split backup escrow for
  `NOVA_MASTER_KEY` versions
- `COLD_STANDBY.md` — manual-failover hot-spare host for
  read-availability after primary failure

Phase 1 will add a complete `docker-compose.yml` and step-by-step
deployment walkthrough.
