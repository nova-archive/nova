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

The Go build injects the version via ldflags:

```sh
go build -ldflags "-X main.buildVersion=$(git describe --tags --always --dirty)" ./cmd/coordinator
```

The `make build-coordinator` target does this automatically (`VERSION`
is computed from `git describe`). At runtime the `NOVA_VERSION`
environment variable overrides the stamped value — container images set
it explicitly so the running version is unambiguous. When neither a
stamp nor the env var is present (e.g. `go run`), the binary reports
`dev`.

## Release checklist (per milestone)

- [ ] Milestone work merged (fast-forward) to its integration point.
- [ ] Annotated tag created with a unique milestone name/version.
- [ ] `ROADMAP.md` milestone row marked complete with the tag name.
- [ ] Coordinator and `nova-node` images, if published, are tagged by
      digest and signed (cosign keyless) — see `.github/workflows/ci.yml`.
