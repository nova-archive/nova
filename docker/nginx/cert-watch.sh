#!/bin/sh
# docker/nginx/cert-watch.sh — TLS bootstrap wait + renewal reload wrapper
# for the prod nginx container.
#
# nginx refuses to start when the cert files named in the wizard-rendered
# /etc/nova/nova.conf do not exist yet. Where they come from depends on the
# tls_mode chosen in the setup wizard:
#
#   dev-self-signed  the wizard wrote the leaf into /etc/nova/tls before the
#                    prod profile ever starts — present on first boot;
#   static           the operator mounts their own PEMs at the paths they
#                    configured;
#   http-01          the certbot sidecar writes a short-lived self-signed
#                    placeholder on first boot (so nginx can start and serve
#                    the ACME challenge on :80), then replaces it with the
#                    real Let's Encrypt certificate — all via the shared
#                    nova-config volume. This container mounts that volume
#                    read-only; certbot owns every write (the nginx image has
#                    no openssl, and the public-facing container keeps no
#                    write access to its own config).
#
# This wrapper therefore:
#   1. derives the configured cert path from the rendered nova.conf (single
#      source of truth — no per-mode wiring in compose),
#   2. waits for the cert to appear (http-01 first boot: a few seconds),
#   3. starts nginx with a background watcher that reloads nginx whenever the
#      cert file's hash changes (initial issuance and every renewal —
#      certbot-loop.sh copies the key first and replaces the cert atomically,
#      so a reload triggered by the cert change never sees a mismatched pair).
#
# The watcher is a no-op for dev-self-signed/static: the file never changes,
# or changes only when the operator rotates it — in which case the reload is
# exactly what they want.
#
# Usage (compose nginx command):  /bin/sh /cert-watch.sh [nova.conf path]
set -eu

CONF="${1:-/etc/nova/nova.conf}"
INTERVAL="${CERT_WATCH_INTERVAL:-60}"

until [ -s "$CONF" ]; do
    echo "cert-watch: waiting for $CONF (run the setup profile first)"
    sleep 5
done

CERT="$(awk '$1 == "ssl_certificate" { gsub(/;/, "", $2); print $2; exit }' "$CONF")"
if [ -z "$CERT" ]; then
    echo "cert-watch: no ssl_certificate directive in $CONF — cannot watch" >&2
    exit 1
fi

until [ -s "$CERT" ]; do
    echo "cert-watch: waiting for $CERT (http-01 mode: certbot writes the bootstrap placeholder)"
    sleep 2
done

last="$(sha256sum "$CERT" | awk '{print $1}')"
(
    while sleep "$INTERVAL"; do
        cur="$(sha256sum "$CERT" 2>/dev/null | awk '{print $1}')"
        if [ -n "$cur" ] && [ "$cur" != "$last" ]; then
            echo "cert-watch: $CERT changed — reloading nginx"
            nginx -s reload || true
            last="$cur"
        fi
    done
) &

exec nginx -g "daemon off;"
