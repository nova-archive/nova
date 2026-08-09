#!/usr/bin/env bash
# scripts/check-docs-cli.sh — every `novactl` command in the documentation must
# be one the current binary can actually parse (P2-M7.2, D-M7.2-10).
#
# The operator documentation shipped commands that could not execute:
# `node ca-init --out-ca-cert/--out-ca-key/--out-coord-cert/--out-coord-key`,
# `node issue --node-id/--ca-cert/--ca-key/--out-cert/--out-key`, and
# `node nebula-template --node-id`. None of those flags exist. That class of
# drift is invisible to review and only shows up when an operator runs it, so
# CI runs every documented invocation through `novactl --check-flags`, which
# parses and stops before doing any work.
#
# Opt out of a single line with a trailing `# novactl-check: ignore` when the
# line is deliberately illustrative rather than runnable.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

bin="$(mktemp -d)/novactl"
trap 'rm -rf "$(dirname "$bin")"' EXIT
go build -o "$bin" ./cmd/novactl

failures=0
checked=0

# Files whose novactl invocations are checked: OPERATOR-FACING docs plus the
# deployment artifact notes — the surfaces an operator copies from.
#
# git ls-files, so untracked and gitignored working files are never gated.
# docs/superpowers/ is excluded: those are historical milestone specs and plans
# recording what was true at the time, not instructions anyone follows today.
mapfile -t files < <(
  git ls-files -- 'docs/**/*.md' 'docs/*.md' 'deploy/**/*.md' 'deploy/*.md' 2>/dev/null \
    | grep -v '^docs/superpowers/' \
    | sort
)

for f in "${files[@]}"; do
  lineno=0
  in_fence=0
  pending=""      # accumulated backslash-continued command
  pending_line=0
  while IFS= read -r raw; do
    lineno=$((lineno + 1))
    line="$raw"

    # Reassemble backslash-continued commands. The ca-init defect is exactly
    # this shape — the flags that do not exist are on continuation lines, so a
    # first-line-only check would miss it entirely.
    if [ -n "$pending" ]; then
      trimmed="${line#"${line%%[![:space:]]*}"}"
      if [ "${trimmed%\\}" != "$trimmed" ]; then
        pending="$pending ${trimmed%\\}"
        continue
      fi
      line="$pending $trimmed"
      pending=""
      lineno=$pending_line
    else
      trimmed="${line#"${line%%[![:space:]]*}"}"
      if [ "${trimmed%\\}" != "$trimmed" ] && [ "$in_fence" -eq 1 ]; then
        pending="${trimmed%\\}"
        pending_line=$lineno
        continue
      fi
    fi

    # Only FENCED code blocks are checked. Prose that quotes a command in
    # inline backticks is not something anyone copy-pastes, and it wraps across
    # lines, which cannot be reassembled reliably.
    case "$line" in
      '```'*|'~~~'*)
        in_fence=$((1 - in_fence))
        continue
        ;;
    esac
    [ "$in_fence" -eq 1 ] || continue

    case "$line" in
      *"novactl-check: ignore"*) continue ;;
    esac

    # Consider lines that invoke novactl directly, AND lines that invoke it via
    # the nova-admin / nova-doctor services, whose image ENTRYPOINT is novactl.
    # The documented federation path uses the service form exclusively, so
    # without this the flagship commands would go unchecked.
    case "$line" in
      *novactl\ *) ;;
      *"run --rm nova-admin "*|*"run --rm nova-doctor "*) ;;
      *) continue ;;
    esac

    # Strip markdown/comment noise and isolate the invocation.
    cmd="${line#"${line%%[![:space:]]*}"}"      # leading whitespace
    cmd="${cmd#\$ }"                              # shell prompt
    cmd="${cmd#docker exec * }"                   # container prefix
    case "$cmd" in
      *novactl\ *) cmd="${cmd##*novactl }" ;;     # direct invocation
      *"run --rm nova-admin "*)  cmd="${cmd##*run --rm nova-admin }" ;;
      *"run --rm nova-doctor "*) cmd="${cmd##*run --rm nova-doctor }" ;;
    esac
    cmd="${cmd%%#*}"                              # trailing comment
    cmd="${cmd%\\}"                               # line-continuation backslash
    cmd="${cmd%"${cmd##*[![:space:]]}"}"          # trailing whitespace
    # Reassembling continuations leaves a leading space, which would make the
    # first token empty and silently skip the command.
    cmd="${cmd#"${cmd%%[![:space:]]*}"}"

    # Skip prose mentions ("run `novactl auth login` to fetch a token"),
    # placeholder-only references, and multi-line continuations we cannot
    # reassemble safely. A backtick anywhere means the line is prose that
    # happens to quote a command, not a runnable block.
    [ -z "$cmd" ] && continue
    case "$cmd" in
      *\`*|\<*) continue ;;
      -*) continue ;;
    esac

    # Only the first token must look like a subcommand.
    first="${cmd%% *}"
    case "$first" in
      auth|signed-url|moderation|keys|setup|upload-token|config|node|collection|pin|federation) ;;
      *) continue ;;
    esac

    checked=$((checked + 1))
    # shellcheck disable=SC2086
    if ! err="$("$bin" --check-flags $cmd 2>&1)"; then
      failures=$((failures + 1))
      echo "FAIL: $f:$lineno" >&2
      echo "      novactl $cmd" >&2
      echo "      -> $(echo "$err" | head -1)" >&2
    fi
  done < "$f"
done

if [ "$failures" -gt 0 ]; then
  echo >&2
  echo "$failures documented novactl invocation(s) the current binary cannot parse." >&2
  echo "Fix the docs (or the CLI). Mark a deliberately illustrative line with" >&2
  echo "a trailing '# novactl-check: ignore'." >&2
  exit 1
fi

echo "OK: $checked documented novactl invocation(s) parse against the current binary"
