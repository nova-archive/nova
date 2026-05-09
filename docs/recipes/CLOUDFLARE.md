# Recipe: Cloudflare CDN fronting

A small operator can absorb roughly 95 % of read traffic on
Cloudflare's free tier by putting it in front of the public read
gateway. Content responses are content-addressed and immutable, so
they cache cleanly and the cache hit rate climbs to near-100 % once
the working set warms up.

This recipe assumes:

- You have a domain on Cloudflare with the relevant subdomain
  pointing at your Nova coordinator's nginx.
- You can edit page rules and cache rules for that zone.
- You do **not** want Cloudflare to terminate TLS using its own
  internal cert (that path adds a Cloudflare-signed cert for your
  hostname to public CT logs; if you care about CT-log exposure,
  see the alternatives section).

## Why this works

The Nova read gateway sets, on every successful response under
`/blob/*` and `/i/*`:

```
Cache-Control: public, max-age=31536000, immutable
```

That means: any standards-compliant cache may serve the response
verbatim for one year without revalidating, regardless of HTTP method
that the client originally used to ask for it. Content-addressed
responses can never need invalidation because changing any byte
would change the URL.

Cloudflare honors `Cache-Control` natively for cacheable file
extensions. For paths without a familiar extension (like
`/i/{cid}`), Cloudflare's default behaviour is to **not** cache; you
need a cache rule to opt in.

## Step by step

### 1. Set up the DNS record

In the Cloudflare DNS dashboard, create an A or AAAA record
pointing at your coordinator's public IP. Set the proxy status to
**proxied** (the orange cloud).

### 2. SSL/TLS mode

Set the SSL/TLS mode to **Full (Strict)**. This requires:

- A valid public certificate on your origin (the cert your nginx
  serves — the same cert you obtained via certbot).
- Cloudflare's edge → origin connection uses TLS and validates the
  origin cert.

Do **not** use the "Flexible" mode; it sends traffic unencrypted
between Cloudflare and your origin, defeating the threat model.

### 3. Cache rules (modern Cache Rules engine)

Create the following cache rules under **Caching → Cache Rules**.
Order matters; Cloudflare evaluates top-to-bottom and the first
match wins.

#### Rule A: cache content endpoints aggressively

- **If incoming requests match:** `URI Path` `starts with` `/i/` or `URI Path` `starts with` `/blob/`
- **Then:**
  - Cache eligibility: **Eligible for cache**
  - Edge TTL: **Use cache-control header if present, otherwise** 1 year
  - Browser TTL: respect origin
  - Cache key: include scheme, host, and full path; ignore query
    string **except** for `sig`, `exp`, `aud`, and `kid` (which
    distinguish signed-URL variants)

The Cache Rules UI lets you toggle "Ignore query string" with an
allowlist; add the four signed-URL parameters as exceptions.

#### Rule B: bypass cache for management endpoints

- **If incoming requests match:** `URI Path` `starts with` any of
  `/api/`, `/admin`, `/legal/`, `/health`, `/metrics`
- **Then:**
  - Cache eligibility: **Bypass cache**

This guarantees authenticated traffic always reaches the origin and
is never served from a stale cache.

#### Rule C: bypass cache for federation endpoints (defense-in-depth)

- **If incoming requests match:** `URI Path` `starts with` `/fed/`
- **Then:**
  - Cache eligibility: **Bypass cache**

In addition, set up a WAF rule blocking `/fed/*` entirely on the
public hostname. The federation endpoint should never be reachable
from the public internet — this is belt-and-suspenders to the
coordinator's interface binding.

### 4. WAF rules

Create the following custom WAF rules at **Security → WAF → Custom rules**:

- **Block `/fed/*`:** action = Block, expression =
  `(http.request.uri.path matches "^/fed/")`
- **Rate-limit aggressive crawlers:** action = Managed Challenge,
  expression depends on your needs; a simple version is "more than
  500 requests per minute from a single IP to `/i/*`."

### 5. Compression

Cloudflare's auto-minify and Brotli compression are safe to enable
for HTML and JSON responses. They have no effect on already-encoded
images (JPEG, WebP, AVIF), which are the bulk of `/i/*` traffic.

Enable Brotli at **Speed → Optimization → Brotli compression**.

### 6. Logging

By default Cloudflare retains some access metadata; the free tier
exposes summary analytics. If your privacy posture requires that no
third party retain access logs, Cloudflare may not be appropriate —
see the alternatives section.

## What the cache looks like

After warm-up:

- A single `/i/{cid}` URL is fetched once from your origin per
  Cloudflare edge location. The popular set typically lives in the
  edge for weeks.
- Range requests are served from cache; Cloudflare assembles the
  range from the cached full object.
- Stale-while-revalidate keeps responses available even when your
  origin has a brief blip.
- A CDN-busting (forced-miss) request via the management UI will
  re-fetch from origin.

Operators with a 1 TB image archive and modest popularity skew
typically see > 95 % cache hit rate after the first week.

## Cost notes

Cloudflare's free tier covers the read-gateway use case for
small-to-medium operators (no per-request billing, generous
bandwidth allowances). Two reasons to consider a paid plan:

- **You exceed the free tier's bandwidth allowance** (most
  community-scale operators do not).
- **You want Argo Smart Routing**, which improves cache hit rate by
  consolidating cache populations across edges. The pricing is
  per-GB and only worth it for high-traffic deployments.

## Alternatives

The Cloudflare path is convenient and cheap, but it routes all
public read traffic through a third party. Operators with stronger
privacy postures may prefer:

- **BunnyCDN.** Similar feature set, similar caching semantics.
  Pay-per-GB, no free tier, but explicit log-retention controls.
- **Self-hosted Varnish or another nginx tier.** No third-party
  involvement; you operate the cache. Higher operational cost.
- **No CDN.** Acceptable for small communities; the read gateway
  serves all traffic. Costs scale linearly with popularity.

If you have CT-log exposure concerns about your hostname (Section
"TLS mode" of `docs/PRIVACY_AUDIT.md`), Cloudflare's edge cert path
is **additional** CT exposure beyond your origin's. The
self-hosted nginx + DNS-01 wildcard route has the smallest
disclosure surface; pick it if your community is privacy-paranoid.

## Verification checklist

- [ ] DNS record is proxied (orange cloud).
- [ ] SSL/TLS mode is Full (Strict).
- [ ] Cache rule for `/i/*` and `/blob/*` is active and uses Edge
      TTL = origin Cache-Control.
- [ ] Bypass rule for `/api/*`, `/admin`, `/legal/*`, `/health`,
      `/metrics` is active.
- [ ] `/fed/*` is blocked by a WAF rule.
- [ ] A test fetch of `/i/{some-cid}` returns `cf-cache-status:
      HIT` after the second request.
- [ ] An authenticated `/api/v1/users/me` request returns `cf-cache-status:
      DYNAMIC` (i.e., bypassed).
- [ ] An access attempt to `/fed/v1/heartbeat` returns 403.
