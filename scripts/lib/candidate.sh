# scripts/lib/candidate.sh — where a gate's CANDIDATE binaries come from
# (P2-M7.3, amendment). Sourced, not executed.
#
# ============================================================================
# The distinction this exists to enforce
# ============================================================================
#
# A gate run has two modes, and conflating them is how a release comes to be
# "verified" by a test that never touched it.
#
#   LOCAL — no descriptors. The candidate is the working tree, built here with
#           the stamping ldflags. This CHARACTERISES the gate: it says what the
#           gate can prove, on a laptop, in minutes.
#
#   RELEASE — descriptors supplied. The candidate is the IMAGE THAT WAS PUSHED,
#           and the binaries are extracted from it by digest. This PROVES the
#           claim for one particular candidate.
#
# Building from the same commit is not the same thing as running the shipped
# bytes. Two builds of one commit are not bit-identical — the build date alone
# is stamped in — and the image also carries an entrypoint, a user, a filesystem
# and a set of runtime libraries that a `go build` on the runner does not. A
# test that downloads a descriptor and then runs its own build has read a JSON
# file, not examined an artifact.
#
# ============================================================================
# Extraction rather than `docker run`
# ============================================================================
#
# The binaries are copied out of the image and run directly. The alternative —
# running each gate's whole topology in containers — would be a larger and more
# faithful test, and it would also be a rewrite of five harnesses that already
# work. Copying gets the property that matters here (these exact bytes, from
# this exact digest) at the cost of the property the container adds (this exact
# filesystem), and the second is what federation-deploy-e2e and the smoke stack
# already cover.
#
# Usage:
#
#	. scripts/lib/candidate.sh
#	nova_candidate_ldflags                 # sets NOVA_CANDIDATE_LDFLAGS
#	nova_candidate_bin coordinator "$WORK/bin/cand-coordinator"
#	echo "$NOVA_CANDIDATE_SOURCE"          # what to print in the gate's summary

# NOVA_CANDIDATE_DESCRIPTORS is the directory of <artifact>.json OCI descriptors
# the release workflow's candidate job wrote. Empty means local mode.
NOVA_CANDIDATE_DESCRIPTORS="${NOVA_CANDIDATE_DESCRIPTORS:-}"
NOVA_CANDIDATE_SOURCE=""
_nova_cand_cache=""

# nova_candidate_ldflags stamps buildinfo for a local build. An unstamped binary
# cannot be the subject of a claim about which artifacts an operator crossed to:
# `upgrade status` reports stamped=false and the gate cannot tell a release
# build from someone's scratch checkout.
nova_candidate_ldflags() {
    local version revision date bi
    version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
    revision="$(git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"
    date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    bi=github.com/nova-archive/nova/internal/buildinfo
    NOVA_CANDIDATE_LDFLAGS="-X $bi.version=$version -X $bi.revision=$revision -X $bi.buildDate=$date"
    export NOVA_CANDIDATE_LDFLAGS
}

# _nova_cand_ref prints the pinned reference for an artifact.
_nova_cand_ref() {
    python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["repository"]+"@"+d["descriptor"]["digest"])' \
        "$NOVA_CANDIDATE_DESCRIPTORS/$1.json"
}

# _nova_cand_unpack copies an image's /usr/local/bin into a cache directory,
# once per image per run. Three binaries live in the coordinator image and
# pulling it three times would triple the slowest step in the release.
_nova_cand_unpack() {
    local artifact="$1" ref dir cid
    [ -n "$_nova_cand_cache" ] || _nova_cand_cache="$(mktemp -d)"
    dir="$_nova_cand_cache/$artifact"
    [ -d "$dir" ] && return 0

    ref="$(_nova_cand_ref "$artifact")" \
        || { echo "candidate: no descriptor for $artifact in $NOVA_CANDIDATE_DESCRIPTORS" >&2; return 1; }
    echo "[candidate] extracting from $ref" >&2
    docker pull --quiet "$ref" >/dev/null || { echo "candidate: cannot pull $ref" >&2; return 1; }
    cid="$(docker create "$ref")" || return 1
    mkdir -p "$dir"
    docker cp "$cid:/usr/local/bin/." "$dir/" >/dev/null 2>&1 \
        || { docker rm -f "$cid" >/dev/null 2>&1; echo "candidate: $ref has no /usr/local/bin" >&2; return 1; }
    docker rm -f "$cid" >/dev/null 2>&1 || true
}

# nova_candidate_bin puts one candidate binary at a path.
#
#	nova_candidate_bin coordinator /tmp/x/cand-coordinator
#
# Known names: coordinator, migrate, novactl (all in the nova-coordinator
# image), and node (in the nova-node image, where it is called nova-node).
nova_candidate_bin() {
    local name="$1" out="$2" artifact inimage pkg
    case "$name" in
        coordinator) artifact=nova-coordinator; inimage=coordinator; pkg=./cmd/coordinator ;;
        migrate)     artifact=nova-coordinator; inimage=migrate;     pkg=./cmd/migrate ;;
        novactl)     artifact=nova-coordinator; inimage=novactl;     pkg=./cmd/novactl ;;
        node)        artifact=nova-node;        inimage=nova-node;   pkg=./cmd/node ;;
        *) echo "candidate: unknown binary $name" >&2; return 1 ;;
    esac

    if [ -z "$NOVA_CANDIDATE_DESCRIPTORS" ]; then
        [ -n "${NOVA_CANDIDATE_LDFLAGS:-}" ] || nova_candidate_ldflags
        NOVA_CANDIDATE_SOURCE="a local stamped build of $(git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)"
        go build -ldflags "$NOVA_CANDIDATE_LDFLAGS" -o "$out" "$pkg"
        return
    fi

    _nova_cand_unpack "$artifact" || return 1
    [ -f "$_nova_cand_cache/$artifact/$inimage" ] \
        || { echo "candidate: $artifact carries no /usr/local/bin/$inimage" >&2; return 1; }
    cp "$_nova_cand_cache/$artifact/$inimage" "$out"
    chmod +x "$out"
    NOVA_CANDIDATE_SOURCE="the pushed candidate images named by $NOVA_CANDIDATE_DESCRIPTORS"
}

# nova_candidate_assert_digests checks that what ran is what was pushed.
#
# The branch above is not evidence. An edit that reordered it, or a descriptor
# directory that was empty, would leave every assertion in the calling gate
# passing about the wrong artifact — and passing loudly, which is worse than
# failing. This asks the local daemon which digest it recorded for the image the
# binaries came out of, and it can only answer correctly if the pull happened.
#
# Prints one line per artifact and returns non-zero on any mismatch.
nova_candidate_assert_digests() {
    local artifact ref want got rc=0
    [ -n "$NOVA_CANDIDATE_DESCRIPTORS" ] || { echo "no descriptors: nothing to assert"; return 0; }
    for artifact in "$@"; do
        ref="$(_nova_cand_ref "$artifact")" || { rc=1; continue; }
        want="${ref##*@}"
        got="$(docker image inspect "$ref" --format '{{join .RepoDigests ","}}' 2>/dev/null || true)"
        case "$got" in
            *"$want"*) echo "  $artifact ran from $want" ;;
            *) echo "  $artifact was NOT run from $want (daemon reports: ${got:-nothing})" >&2; rc=1 ;;
        esac
    done
    return "$rc"
}
