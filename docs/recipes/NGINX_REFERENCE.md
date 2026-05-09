# Recipe: Annotated nginx reference configuration

Walks through `nginx/nova.conf.example` line by line, explaining
why each directive is set the way it is and which choices the
operator should adapt to their environment.

## Context

The reference config assumes:

- nginx 1.25 or newer.
- The Nova coordinator is reachable on `127.0.0.1:8080` (loopback)
  or on a Unix domain socket. The reference uses TCP for clarity.
- Certbot has already obtained a certificate (or the operator
  supplied one statically). See `OPERATOR_CHECKLIST.md` for the
  TLS-mode choice.
- The file lands in `/etc/nginx/conf.d/nova.conf`, which is
  included inside `http { }` automatically by the default nginx
  layout.

If your nginx has a different convention (e.g., `sites-available`
+ `sites-enabled` symlinks), adjust accordingly. The directives
themselves are the same.

## Rate-limit zones

```nginx
limit_req_zone $binary_remote_addr zone=nova_per_ip:10m rate=30r/s;
limit_req_zone $binary_remote_addr zone=nova_uploads:10m rate=5r/s;
limit_req_zone $nova_user_key zone=nova_per_user:10m rate=60r/s;
```

Three zones because Nova has three traffic shapes:

1. **`nova_per_ip`** — anonymous reads. 30 req/s sustained per IP
   is generous for normal browsing but tightens up bots that walk
   the catalog. Burst is set per-location.
2. **`nova_uploads`** — uploads. Stricter (5 req/s) because a single
   upload can consume seconds of CPU and memory; rate-limit at the
   request granularity, not the byte.
3. **`nova_per_user`** — authenticated management calls. Looser
   (60 req/s) because authenticated users have a real cost
   relationship with the operator.

The `$nova_user_key` map at the top of the file extracts the bearer
token from the `Authorization` header and uses it as the rate-limit
key when present. Unauthenticated traffic falls back to IP. This
gives a per-user quota for authenticated callers and a per-IP
quota for anonymous ones.

**Memory budget.** Each zone reserves 10 MB of shared memory; nginx
docs estimate ~16,000 unique keys per MB, so 10 MB ≈ 160,000 unique
IPs/tokens before the zone overflows. For larger deployments,
increase to 50 MB or 100 MB.

## Content cache

```nginx
proxy_cache_path /var/cache/nginx/nova
                 levels=1:2
                 keys_zone=nova_content:64m
                 max_size=2g
                 inactive=7d
                 use_temp_path=off;
```

Nova's `/blob/*` and `/i/*` responses set `Cache-Control: public,
max-age=31536000, immutable`. We honor it.

- **`max_size=2g`**: a small home-tier nginx caches 2 GB of
  content. Operators with disk-rich edges should raise this; the
  hit rate improves with the cache size. A 100 GB edge cache for a
  1 TB working set is a reasonable target.
- **`inactive=7d`**: a cached object that has not been hit for 7
  days is evicted. Tune up for archival workloads, down for
  highly-skewed popularity.
- **`use_temp_path=off`**: write directly into the cache directory
  rather than staging in a temp directory. Avoids cross-filesystem
  copies; the cache directory must be on the same filesystem as
  nginx's temp area.
- **`keys_zone=nova_content:64m`**: 64 MB of shared memory holds
  ~1 million cache keys. Most operators stay under 500 K active
  CIDs.

## TLS

```nginx
ssl_protocols TLSv1.2 TLSv1.3;
ssl_ciphers ECDHE+AESGCM:ECDHE+CHACHA20:ECDHE+AES;
```

Modern, conservative cipher suite. TLS 1.0 and 1.1 are deprecated
and not enabled. The ChaCha20 inclusion is for clients without
hardware AES (mobile, low-power devices).

```nginx
ssl_session_tickets off;
```

Forward secrecy without ticket-based key reuse. Slightly more
expensive on the handshake but the right default for privacy.

```nginx
ssl_stapling on;
ssl_stapling_verify on;
resolver 1.1.1.1 9.9.9.9 valid=300s;
```

OCSP stapling means clients verify cert revocation through your
nginx, not by reaching out to the CA themselves. Using Cloudflare
and Quad9 as resolvers; substitute your own privacy-respecting
resolvers if you have a stronger threat model.

## Security headers

```nginx
add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
add_header X-Content-Type-Options "nosniff" always;
add_header X-Frame-Options "DENY" always;
add_header Referrer-Policy "strict-origin-when-cross-origin" always;
add_header Permissions-Policy "interest-cohort=(), browsing-topics=()" always;
```

Standard hardening:

- **HSTS** with one-year `max-age` and `includeSubDomains`. Note
  that `includeSubDomains` is a commitment; if you ever serve a
  subdomain over plain HTTP, browsers will refuse it.
- **`nosniff`** prevents MIME-type guessing.
- **`X-Frame-Options: DENY`** stops the admin SPA from being
  framed by anyone. The CSP `frame-ancestors 'none'` is the
  modern equivalent; we set both for browser-compatibility coverage.
- **`Referrer-Policy: strict-origin-when-cross-origin`** sends the
  origin (no path) on cross-origin requests.
- **`Permissions-Policy`** opts out of FLoC / Topics
  fingerprinting by default.

```nginx
add_header Content-Security-Policy
    "default-src 'self'; img-src 'self' data: blob:; ...";
```

The CSP is restrictive because the admin SPA build is hermetic.
`img-src` allows `data:` and `blob:` for the upload-preview flow
(Uppy uses both). `style-src 'unsafe-inline'` is unfortunately
required by the React stack at present; we accept it as the
primary risk surface and mitigate by hermetic build (no third-party
JS, no inline scripts).

## Body size and timeouts

```nginx
client_max_body_size 100m;
```

100 MB upload limit. Tune to your operator's
`max_upload_size` config; the value here must be ≥ the
coordinator's value or uploads will fail at nginx before reaching
Nova's tus handler.

For tus uploads in particular:

```nginx
proxy_request_buffering off;
proxy_buffering off;
```

Set per-location for upload paths. Without these, nginx buffers the
entire upload before forwarding, defeating tus's resumable nature
and using a lot of disk.

## Per-location structure

The reference config has six location blocks plus a default. The
order is "most specific first" because nginx matches regex
locations in declaration order:

1. `= /health` — exact match, fast path, no rate limit
2. `~ ^/(blob|i)/` — content with caching
3. `~ ^/api/v1/(uploads|blobs|images)(/|$)` — upload paths,
   streamed
4. `/api/v1/` — other management endpoints, per-user limit
5. `/legal/` — public legal endpoints
6. `/admin` — admin SPA
7. `/fed/` — explicitly blocked at the public proxy
8. `/metrics` — IP-restricted
9. `/` — default

If you add an endpoint, slot it into the right place in this order.

## /fed/ blocking

```nginx
location /fed/ {
    return 404;
}
```

Defense-in-depth. The coordinator's federation endpoint binds only
to the Nebula interface, so it would never serve a public request
even if nginx forwarded one. We block at the public proxy anyway —
"defense in depth means the second layer never has to fire."

If your nginx serves both the public hostname and a private
hostname (e.g., `coordinator.nebula:8080`), put the public-facing
nginx and the private-facing nginx in separate `server { }` blocks
listening on different interfaces; the public one keeps the `/fed/`
block and the private one omits it.

## /metrics ACL

```nginx
location /metrics {
    allow 127.0.0.1;
    # allow 10.0.0.0/8;
    deny all;
    ...
}
```

Prometheus scrapers usually live on a private network. The default
denies the public internet; uncomment the RFC 1918 ranges that
match your scraper's network.

If your scraper authenticates via bearer token, you can drop the
ACL entirely and rely on the token. The IP-based ACL is a safe
default that does not require additional setup.

## Common operator customizations

### Separate hostname for /admin

```nginx
server {
    listen 443 ssl;
    server_name admin.example.com;
    # ... TLS, headers ...

    location / {
        proxy_pass http://nova_coordinator/admin/;
    }
}
```

Move the admin SPA to its own hostname to allow stricter access
control (IP-restricted, mTLS, etc.) without affecting the public
content surface. Drop the `/admin` location from the public
hostname's server block.

### Different cache directory

For SSD-equipped hosts, point the cache at a fast filesystem:

```nginx
proxy_cache_path /mnt/ssd/nginx-cache ...;
```

Make sure the mount is the same filesystem as `proxy_temp_path`,
or set `use_temp_path=off` (which the reference does).

### Larger uploads

For a `nova-archive` deployment hosting multi-GB datasets, raise
both `client_max_body_size` and the coordinator's
`max_upload_size`. tus chunk size remains 5 MiB regardless.

## Validation checklist

- [ ] `nginx -t` passes after edits.
- [ ] `curl -I https://{host}/health` returns `200`.
- [ ] `curl -I https://{host}/fed/v1/heartbeat` returns `404`.
- [ ] `curl -I https://{host}/i/{cid}` returns `Cache-Control:
      public, max-age=31536000, immutable`.
- [ ] Rapid `curl` against `/api/v1/uploads` from one IP triggers
      `429 Too Many Requests` after the burst quota.
- [ ] `curl https://{host}/metrics` from outside an allowed range
      returns `403`.
- [ ] HSTS header present and TLS 1.2 minimum (test with
      `testssl.sh` or SSL Labs).
