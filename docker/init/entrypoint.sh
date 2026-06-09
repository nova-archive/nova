#!/bin/sh
set -eu

NOVA_CONFIG_DIR="${NOVA_CONFIG_DIR:-/etc/nova}"
NOVA_SECRETS_DIR="${NOVA_SECRETS_DIR:-/run/secrets}"
NOVA_KUBO_REPO="${NOVA_KUBO_REPO:-/var/lib/nova/kubo}"
NOVA_UPLOAD_TMP_DIR="${NOVA_UPLOAD_TMP_DIR:-/var/tmp/nova-uploads}"

# Running as root: make the mounted (root-owned) volumes writable by the
# non-root nova user, then drop privileges. The coordinator process must NOT
# run as root (startup floor).
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$NOVA_CONFIG_DIR" "$NOVA_SECRETS_DIR" "$NOVA_KUBO_REPO" "$NOVA_UPLOAD_TMP_DIR"
  chown -R nova:nova "$NOVA_CONFIG_DIR" "$NOVA_SECRETS_DIR" "$NOVA_KUBO_REPO" "$NOVA_UPLOAD_TMP_DIR"
fi

# Forward-only migrations (embedded; idempotent).
gosu nova /usr/local/bin/migrate up

if [ -f "$NOVA_CONFIG_DIR/.bootstrap-complete" ]; then
  echo "entrypoint: .bootstrap-complete present -> normal mode"
else
  echo "entrypoint: .bootstrap-complete absent -> SETUP mode (/setup only, loopback :8444)"
fi

exec gosu nova /usr/local/bin/coordinator
