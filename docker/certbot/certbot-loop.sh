#!/bin/sh
# docker/certbot/certbot-loop.sh — ACME http-01 issuance + renewal loop for
# the prod certbot sidecar.
#
# Runs only useful work when the wizard rendered an http-01 nova.conf (the
# ACME challenge location is the mode marker); for dev-self-signed/static the
# loop is a 12-hourly no-op. Everything is (re-)derived from the rendered
# config each cycle, so the loop self-heals if it starts before the wizard
# has run.
#
# Per cycle, in http-01 mode:
#   1. PLACEHOLDER  If the configured leaf is missing (first boot, before any
#      issuance), write a short-lived self-signed placeholder so nginx can
#      start and serve the ACME challenge on :80. This runs here — not in the
#      nginx container — because the nginx alpine image ships no openssl and
#      mounts /etc/nova read-only; this container already owns the writes.
#   2. ISSUE        If no Let's Encrypt lineage exists for the hostname yet,
#      run `certbot certonly --webroot` (initial issuance). Failures are
#      logged and retried next cycle (LE rate limits make 12h a safe cadence).
#   3. RENEW        `certbot renew` — idempotent, skips certs not yet due.
#   4. DEPLOY       Copy the live lineage over the paths nova.conf points at
#      when it differs: key first, then the cert via tmp+rename, so the
#      cert-change signal that nginx's cert-watch.sh reloads on never exposes
#      a mismatched pair. This volume-mediated copy is the cross-container
#      "deploy hook": no signalling between containers is needed.
#
# Hostname / contact email default to the wizard's answers (server_name in
# nova.conf, contact_email in operator.yaml); NOVA_HOSTNAME and
# NOVA_CONTACT_EMAIL in docker/.env override them.
set -u

CONF=/etc/nova/nova.conf
OPERATOR_YAML=/etc/nova/operator.yaml
WEBROOT=/var/lib/certbot/webroot
SLEEP_SECS=43200 # 12h

trap 'exit 0' TERM INT

log() { echo "certbot-loop: $*"; }

conf_value() { # conf_value <directive> — first value of an nginx directive
    awk -v d="$1" '$1 == d { gsub(/;/, "", $2); print $2; exit }' "$CONF"
}

yaml_contact_email() {
    sed -n 's/^[[:space:]]*contact_email:[[:space:]]*//p' "$OPERATOR_YAML" 2>/dev/null | head -n 1
}

while :; do
    if [ -s "$CONF" ] && grep -q "acme-challenge" "$CONF"; then
        CERT="$(conf_value ssl_certificate)"
        KEY="$(conf_value ssl_certificate_key)"
        HOST="${NOVA_HOSTNAME:-$(conf_value server_name)}"
        EMAIL="${NOVA_CONTACT_EMAIL:-$(yaml_contact_email)}"
        LIVE="/etc/letsencrypt/live/$HOST"

        if [ -z "$CERT" ] || [ -z "$KEY" ] || [ -z "$HOST" ]; then
            log "could not derive cert/key/hostname from $CONF — retrying next cycle"
        else
            # 1. Placeholder so nginx can start and serve the challenge.
            if [ ! -s "$CERT" ]; then
                log "$CERT missing — writing self-signed placeholder for $HOST"
                mkdir -p "$(dirname "$CERT")" "$(dirname "$KEY")"
                openssl req -x509 -newkey rsa:2048 -nodes -days 7 \
                    -keyout "$KEY" -out "$CERT" -subj "/CN=$HOST" \
                    -addext "subjectAltName=DNS:$HOST" 2>/dev/null
                chmod 600 "$CERT" "$KEY"
            fi

            # 2. Initial issuance (skipped once a live lineage exists).
            if [ ! -s "$LIVE/fullchain.pem" ]; then
                sleep 15 # let nginx pick up the placeholder and bind :80
                log "no certificate lineage for $HOST — attempting initial issuance"
                certbot certonly --webroot -w "$WEBROOT" -d "$HOST" \
                    --email "$EMAIL" --agree-tos --no-eff-email --non-interactive ||
                    log "initial issuance failed (will retry next cycle)"
            fi

            # 3. Renewal — idempotent, skips certs not yet due (<30d).
            certbot renew --webroot -w "$WEBROOT" --quiet || true

            # 4. Deploy the live lineage over the configured paths. Key first,
            #    cert atomically last: nginx's watcher keys off the cert.
            if [ -s "$LIVE/fullchain.pem" ] && ! cmp -s "$LIVE/fullchain.pem" "$CERT"; then
                log "deploying certificate for $HOST to $CERT"
                cp -fL "$LIVE/privkey.pem" "$KEY.tmp" &&
                    chmod 600 "$KEY.tmp" && mv -f "$KEY.tmp" "$KEY"
                cp -fL "$LIVE/fullchain.pem" "$CERT.tmp" &&
                    chmod 600 "$CERT.tmp" && mv -f "$CERT.tmp" "$CERT"
            fi
        fi
    fi
    sleep "$SLEEP_SECS" &
    wait $!
done
