#!/bin/sh
set -eu

NOVA_CONFIG_DIR="${NOVA_CONFIG_DIR:-/etc/nova}"
NOVA_SECRETS_DIR="${NOVA_SECRETS_DIR:-/run/secrets}"
NOVA_KUBO_REPO="${NOVA_KUBO_REPO:-/var/lib/nova/kubo}"
NOVA_UPLOAD_TMP_DIR="${NOVA_UPLOAD_TMP_DIR:-/var/tmp/nova-uploads}"
NOVA_UPGRADE_JOURNAL_DIR="${NOVA_UPGRADE_JOURNAL_DIR:-/var/lib/nova/upgrade}"
export NOVA_UPGRADE_JOURNAL_DIR

# Running as root: make the mounted (root-owned) volumes writable by the
# non-root nova user, then drop privileges. The coordinator process must NOT
# run as root (startup floor).
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$NOVA_CONFIG_DIR" "$NOVA_SECRETS_DIR" "$NOVA_KUBO_REPO" "$NOVA_UPLOAD_TMP_DIR" \
           "$NOVA_UPGRADE_JOURNAL_DIR"
  chown -R nova:nova "$NOVA_CONFIG_DIR" "$NOVA_SECRETS_DIR" "$NOVA_KUBO_REPO" \
                     "$NOVA_UPLOAD_TMP_DIR" "$NOVA_UPGRADE_JOURNAL_DIR"
  # The journal records what an upgrade did, including to a database that then
  # failed. It is not world-readable.
  chmod 0700 "$NOVA_UPGRADE_JOURNAL_DIR"
fi

# Forward-only migrations, applied unattended ONLY when the whole pending range
# is safe to apply unattended (P2-M7.3, D-M7.3-9a).
#
# `migrate up` ran every pending migration with nobody watching. That is fine
# for the additive ones it has seen and wrong for the ones it has not: 0003
# drops two tables, 0009 takes a write lock on a corpus-scale index. An
# unattended container is not a person who read the release notes.
#
# `migrate auto` applies only an old-binary-compatible, online, maintenance-free
# range with no operator procedures, and otherwise prints the exact command and
# exits ZERO. Exiting non-zero would crash-loop the coordinator over a decision
# that is waiting on a human — turning "your upgrade needs attention" into "your
# archive is down" — and the coordinator's own startup floor refuses a stale
# schema a few lines later, which is the failure that should stop it.
gosu nova /usr/local/bin/migrate auto

if [ -f "$NOVA_CONFIG_DIR/.bootstrap-complete" ]; then
  echo "entrypoint: .bootstrap-complete present -> normal mode"
else
  echo "entrypoint: .bootstrap-complete absent -> SETUP mode (/setup only, loopback :8444)"
fi

exec gosu nova /usr/local/bin/coordinator
