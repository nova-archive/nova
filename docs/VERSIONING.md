# Versioning

Nova follows [semantic versioning](https://semver.org/) and treats
**every milestone as a distinct, uniquely versioned release**. No two
variations of Nova — however small the change — carry the same version
number.

## Principles

1. **Unique version per build.** Release artifacts derive their version
   from `git describe --tags --always --dirty`. A build from a tagged
   commit reports that tag; a build from an untagged commit reports the
   nearest tag plus the commit count and short SHA; a build from a dirty
   working tree is suffixed `-dirty`. Two materially different trees
   cannot report the same version.
2. **A tag per milestone.** Each milestone is an annotated tag (e.g.
   `m14-polish-release`, `p2-m2-identity-registration`). The milestone
   history in [`ROADMAP.md`](ROADMAP.md) is authoritative for what each
   tag contains.
3. **Pre-1.0 semantics.** While Nova is `0.x`, the minor version may
   change with breaking changes; milestone tags are the stable
   reference points. The `v0.1.0-rc1` tag marks the Phase 1 release
   candidate.
4. **Four independent version axes** (see the Phase 2 federation
   design): coordinator software, donor (`nova-node`) software, the
   `fed/vN` federation protocol (the real interop contract), and the
   `NOVE` envelope format. These move independently; the root Go module
   stays at `v0.x`.

## How the version is stamped

Every binary reads `internal/buildinfo`, and the Go build injects all
three values via ldflags:

```sh
BI=github.com/nova-archive/nova/internal/buildinfo
go build -ldflags "-X $BI.version=$(git describe --tags --always --dirty) \
                   -X $BI.revision=$(git rev-parse --short=7 HEAD) \
                   -X $BI.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/coordinator
```

`$(GO_LDFLAGS)` in the Makefile carries all three, and **every** build
target uses it. The three Dockerfiles take `NOVA_VERSION`,
`NOVA_REVISION` and `NOVA_BUILD_DATE` as build args, pass them to the
same flags, and set the matching `org.opencontainers.image.*` labels, so
an image and the binaries inside it cannot disagree. `--version` on any
binary prints what it was stamped with.

An unstamped build (`go run`, a bare `go build`) reports `dev` /
`unknown` rather than an empty string, so a census can tell "this is a
developer build" from "this field was never populated".

**There is no runtime override.** `NOVA_VERSION` used to outrank the
stamp at runtime; as of P2-M7.3 it does nothing. An environment variable
that can lie about immutable build information has no legitimate use
once a binary is stamped, and the fleet census must not be able to
launder a claim through one.

## Three tag namespaces, and what each names

They are different kinds of thing and are routinely confused, so they are
written out.

| Namespace | Example | Names | Mutable? |
|---|---|---|---|
| **Git milestone tag** | `p2-m7.1-beta-readiness` | a commit, in this repository's history | no, by convention |
| **Git product tag** | `v0.3.0` | the commit a release was cut from | no — created only by `release.yml`, after the promoted digests read back |
| **OCI tag** | `ghcr.io/nova-archive/nova-node:v0.3.0` | a manifest in a registry | **YES** — anyone with push access can repoint it |

Nothing in Nova resolves an OCI tag at deploy time. Every operational reference
is `repository@sha256:...`, and the release lock carries OCI **descriptors**
— digest, mediaType, size — because a bare digest cannot say whether it names an
index or a single manifest, and promotion behaves differently for the two.

The OCI version tag exists so a human browsing the registry can find the
release. It is never a deployment input.

## The baseline is a commit

Nova's first contract-bearing release upgrades FROM a deployment that has no
product version: the remote tag namespace holds only pre-contract milestone
refs, and the thing running in the field is a local build of commit `143c459`.

A model that can only name versions cannot describe the one transition that
actually has to work. So a supported predecessor carries a **kind** — `commit`
or `version` — and renders as `commit:143c459` or `v0.3.0`. The cross-version
and schema gates read that ref from the reviewed intent rather than from a shell
variable somebody has to remember to bump.

## Intent and lock

Two documents, because one cannot do both jobs.

**Intent** (`releases/intent/vX.Y.Z.json`) is reviewed BEFORE any build exists.
It declares the version, the target schema, the platforms, the supported
predecessors, the capability profiles, the repositories, and the claims the
release wants to make — with the gate that must prove each. It carries no
digests and no evidence, because neither exists yet. Its claim type has no
evidence field at all, so a declared claim cannot acquire proof by being
assigned to the wrong variable.

**The lock** (`lock.json`) is produced after the build and signed. It records
the exact intent bytes it realizes, the source commit, the artifact descriptors,
the sidecars, the proven claims with their evidence, the donor-lock projection's
digest, and a payload map covering every bundle member.

The **embedded catalog** is generated from the intent and compiled into every
binary, which is what makes `novactl upgrade status` work offline. It carries no
digests: a self-referential digest constant in the source tree is exactly the
circularity this split exists to break.

### A rebuild will not reproduce the digest

Stated plainly because it surprises people: **rebuilding the same commit after
the lock was written does not produce the image the lock names.**

Binaries are stamped with version, revision and build date through linker
flags, and the images carry matching OCI labels. The build date alone
guarantees a different image config, which guarantees a different manifest
digest. Reproducibility of the digest was never the property — verifiability
was: you check the signature and the provenance attestation on the digest the
lock names, not by rebuilding and comparing.

That is also why the release workflow builds ONCE and then promotes. A rebuild
to produce a "release" image would change the digest the lock already committed
to.

## Release checklist (per milestone)

- [ ] Milestone work merged (fast-forward) to its integration point.
- [ ] Annotated tag created with a unique milestone name/version.
- [ ] `ROADMAP.md` milestone row marked complete with the tag name.
- [ ] Coordinator and `nova-node` images, if published, are tagged by
      digest and signed (cosign keyless) — see `.github/workflows/ci.yml`.
- [ ] Cross-version gate green: `make crossversion-e2e PAIRING=all`
      (N−1 × HEAD binary matrix, P2-M7 D-M7-3).
- [ ] Corpus benchmark artifact recorded: `make bench-corpus` against the
      committed release thresholds; commit the
      `reports/benchmarks/p2-m7-corpus-<date>.{json,md}` artifact (D-M7-2).
