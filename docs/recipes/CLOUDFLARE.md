# Recipe: Cloudflare CDN fronting (optional, with significant caveats)

> **Read first.** This recipe is **optional**, not recommended for
> all deployments. Default Nova posture is no third-party traffic
> intermediation: the coordinator serves all reads itself, donor
> nodes replicate ciphertext, and no public CDN is in the request
> path. That is the privacy-paranoid baseline.
>
> Putting a third-party CDN in front of Nova introduces a real
> tradeoff: in any "proxied" mode, the CDN terminates TLS at its
> edge and therefore **sees plaintext** on every request and
> response, regardless of Nova's donor-blind storage encryption.
> Nova's encryption protects donor nodes from seeing user content;
> it does not protect the CDN edge from seeing it. If your
> deployment's privacy posture cares about that — and many private
> communities will — do not enable CDN proxying.
>
> This document explains when CDN fronting may make sense, what its
> privacy cost is, and how to configure it if you choose to.

## When CDN fronting makes sense

- The deployment is a **high-traffic public archive** where
  bandwidth costs at the origin would otherwise be prohibitive.
- The content served is genuinely public (covers, OG images,
  derivative thumbs) and you have no privacy concern about the CDN
  caching it.
- You accept that CDN edges are a third party in the request path
  and that any privacy claim about Nova's read path is bounded by
  the CDN's policies and incident history.
- The deployment is small enough that the marginal financial
  benefit of CDN caching outweighs the additional moving piece in
  the architecture.

## When CDN fronting does NOT make sense

- The deployment serves private collections or signed-URL content
  where users expect their reads to remain between them and the
  operator.
- The community's threat model includes "no third-party
  intermediation" (most private communities and privacy-focused
  fediverse instances).
- Bandwidth costs at the origin are tolerable and the network is
  small enough that a CDN's complexity is not justified.
- The deployment runs in `paranoid: true` mode (see
  `docs/PRIVACY_AUDIT.md` § "paranoid mode").

For these cases, **no CDN at all** is the correct architecture. The
coordinator's read gateway with proper rate-limiting (see
`nginx/nova.conf.example`) handles community-scale traffic
adequately.

## The Cloudflare proxy / decryption-on-ingress problem

Cloudflare's "proxied" mode (the orange cloud icon in the DNS
panel) routes user requests through Cloudflare's edge before they
reach the origin. The user's TLS connection terminates at
Cloudflare; Cloudflare opens a fresh TLS connection to the origin
and forwards the decrypted bytes.

The consequence: Cloudflare reads every URL path, every header,
every response body, and every Range request. For Nova, that means:

- Cloudflare sees the signed-URL `sig`, `aud`, `kid`, `exp`
  parameters of every authenticated read.
- Cloudflare sees the **decrypted** image bytes the read gateway
  serves (Nova's encryption envelope is unwrapped at the
  coordinator before nginx, and the bytes leaving nginx are
  plaintext).
- Cloudflare's logs record every request path. Cloudflare's
  retention policy and access controls govern those logs, not
  Nova's.

This is true of any CDN that operates in proxy mode (BunnyCDN,
Fastly, AWS CloudFront, Akamai). The cryptographic donor-blindness
Nova provides does not extend across this layer.

If your privacy claim to your community is "we don't put your
content in front of third parties," do not enable a proxying CDN.

## Mitigations if you do enable Cloudflare

If you accept the tradeoff, the configuration below minimizes the
exposure surface.

### 1. DNS configuration

In the Cloudflare DNS dashboard, create an A or AAAA record
pointing at your coordinator's public IP. **Two options:**

- **DNS-only (grey cloud):** Cloudflare resolves DNS but does not
  proxy traffic. **No content passes through Cloudflare.** This is
  the privacy-preserving option, with no caching benefit. Use it
  if you want Cloudflare's DDoS protection at the DNS layer
  without giving them visibility into request bodies.
- **Proxied (orange cloud):** Cloudflare proxies, caches, and
  inspects all traffic. Use only if you have explicitly accepted
  the privacy tradeoff above.

### 2. SSL/TLS mode (proxied path only)

Set the SSL/TLS mode to **Full (Strict)**.

- Requires a valid public certificate on your origin (the cert
  your nginx serves — the same cert you obtained via certbot).
- Cloudflare's edge → origin connection uses TLS and validates the
  origin cert.

Do **not** use the "Flexible" mode; it sends traffic unencrypted
between Cloudflare and your origin.

### 3. Authenticated Origin Pulls

Cloudflare offers Authenticated Origin Pulls (AOP), a feature
where Cloudflare presents a Cloudflare-signed client certificate
to your origin, and your nginx verifies it before serving. This
prevents anyone other than Cloudflare's edge from reaching your
origin.

This does NOT change Cloudflare's plaintext visibility — they
still terminate TLS at the edge — but it does prevent third
parties from bypassing Cloudflare to reach your origin directly.

Enable in the Cloudflare panel under SSL/TLS → Origin Server →
Authenticated Origin Pulls. Configure nginx to require the
Cloudflare client cert.

### 4. Cache rules (proxied path only)

Cloudflare Cache Rules under **Caching → Cache Rules**. Order
matters; the first match wins.

#### Rule A: cache content endpoints aggressively (only for genuinely public)

- **If incoming requests match:** `URI Path` `starts with` `/i/`
  AND `URI Query` does not contain `sig=` (i.e., no signed URL).
  This restricts caching to anonymous reads of public collections.
- **Then:** Eligible for cache; Edge TTL = origin Cache-Control;
  Cache key includes the full path (no signed-URL parameters).

#### Rule B: never cache signed URLs

- **If incoming requests match:** `URI Query` `contains` `sig=`
- **Then:** Bypass cache. Signed URLs are private content — caching
  them at a shared edge defeats the access control.

#### Rule C: never cache management or federation paths

- **If incoming requests match:** `URI Path` `starts with` any of
  `/api/`, `/admin`, `/legal/`, `/health`, `/metrics`, `/fed/`
- **Then:** Bypass cache.

#### Rule D: WAF block /fed/* on the public hostname

- Custom WAF rule: `(http.request.uri.path matches "^/fed/")` →
  Block. The federation endpoint is supposed to be reachable only
  over Nebula. This is defense-in-depth.

### 5. Logging and retention

Cloudflare logs request metadata for some retention period
depending on plan. Free-tier accounts get summary analytics; paid
plans expose Logpush. **Treat Cloudflare's logs as a third-party
data store** subject to their policies, not yours.

If your privacy posture requires no third-party logs, do not enable
a proxying CDN.

### 6. Cache purge on takedown

When a takedown action runs (quarantine or tombstone), Nova emits
the `image.flagged` and `takedown.actioned` webhooks. Operators
running Cloudflare should subscribe a webhook handler that calls
Cloudflare's purge-by-URL or purge-by-tag API to evict cached
copies of the affected CID.

Without purge integration, Cloudflare may continue serving cached
plaintext for the configured Edge TTL even after Nova has
quarantined or tombstoned the underlying blob. **This is the most
operationally dangerous gap in the CDN-fronted configuration** and
it must be addressed before public deployment.

A reference handler skeleton lives at
`nova-cloudflare-purge` (separate repo, Phase 4 deliverable).
Until then, operators implement the purge call themselves.

## Verification checklist (proxied configuration only)

- [ ] DNS record proxy mode is the deliberate choice (grey cloud
      for privacy-preserving; orange cloud for caching with the
      privacy tradeoff understood).
- [ ] SSL/TLS mode is Full (Strict) when proxied.
- [ ] Authenticated Origin Pulls enabled (proxied) so direct-to-
      origin requests fail.
- [ ] Cache rule for `/i/*` excludes paths with `sig=`.
- [ ] Bypass rule for `/api/*`, `/admin`, `/legal/*`, `/health`,
      `/metrics`, `/fed/*` is active.
- [ ] WAF rule blocks `/fed/*` entirely on the public hostname.
- [ ] Cache-purge webhook handler is configured and tested.
- [ ] A test fetch of `/i/{public-cid}` returns
      `cf-cache-status: HIT` after the second request.
- [ ] A signed-URL request returns `cf-cache-status: BYPASS`.
- [ ] A `/fed/v1/heartbeat` request returns 403.
- [ ] A test takedown triggers cache purge within the SLA.

## Alternatives

If CDN-fronting is not appropriate for your deployment but you do
want some traffic offload:

- **Self-hosted Varnish or another nginx tier.** No third-party
  involvement; you operate the cache. Higher operational cost.
- **DNS-only Cloudflare** (grey cloud) for DDoS protection at the
  DNS layer without proxying — no caching benefit, but no plaintext
  exposure either.
- **No CDN.** For most community-scale operators, this is the
  right answer. The coordinator's read gateway with proper
  rate-limiting handles modest traffic adequately, and you preserve
  the privacy story end-to-end.

## Final note

A CDN is one of the most consequential architectural decisions an
operator makes. It can reduce origin bandwidth costs by 95 % or
more on cacheable workloads. It also introduces a third party into
every request path, with their own logs, their own retention, and
their own subpoena exposure.

Nova does not require a CDN. The coordinator is the system of
record and the read gateway. Operators choose CDN integration
deliberately, with full understanding of the privacy tradeoff —
not as a default.
