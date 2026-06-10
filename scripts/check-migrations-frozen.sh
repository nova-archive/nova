#!/usr/bin/env bash
# scripts/check-migrations-frozen.sh — shipped goose migrations are immutable.
#
# Verifies both drift directions against internal/db/migrations/MANIFEST.sha256:
#   1. every file listed in the manifest exists and hash-matches (no edits);
#   2. every NNNN_*.sql migration on disk is listed (no unlisted migrations).
#
# Adding a new migration 00NN_x.sql:
#   (cd internal/db/migrations && sha256sum 00NN_x.sql >> MANIFEST.sha256)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
MIG_DIR="internal/db/migrations"
MANIFEST="MANIFEST.sha256"

if [[ ! -f "$MIG_DIR/$MANIFEST" ]]; then
    echo "FAIL: $MIG_DIR/$MANIFEST missing" >&2
    exit 1
fi

# Direction 1: listed files unchanged.
if ! (cd "$MIG_DIR" && sha256sum --check --strict --quiet "$MANIFEST"); then
    echo "FAIL: a shipped migration was edited or deleted." >&2
    echo "Shipped migrations are forward-only and immutable; write a new" >&2
    echo "migration instead of editing an applied one." >&2
    exit 1
fi

# Direction 2: every migration on disk is listed.
status=0
for f in "$MIG_DIR"/[0-9][0-9][0-9][0-9]_*.sql; do
    base="$(basename "$f")"
    if ! grep -qE "^[0-9a-f]{64}  $base$" "$MIG_DIR/$MANIFEST"; then
        echo "FAIL: $base is not in $MANIFEST — append it:" >&2
        echo "  (cd $MIG_DIR && sha256sum $base >> $MANIFEST)" >&2
        status=1
    fi
done
exit "$status"
