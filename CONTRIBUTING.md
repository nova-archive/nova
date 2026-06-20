# Contributing to Nova

Thanks for your interest in contributing. Nova is in **active
development**: Phase 0 (specifications) and Phase 1 (single-node MVP)
are complete, and Phase 2 (federation + streaming-AEAD) is underway —
see [`docs/ROADMAP.md`](docs/ROADMAP.md) for current milestone status.
Both code and specification contributions are welcome. The protocol
and data-model contracts under `docs/specs/` are deliberately
constrained, so discuss changes there before opening a PR (see below).

## How to contribute

1. **Discuss first.** For any non-trivial change, open an issue
   describing the problem and your proposed approach before writing a
   PR. The architecture is intentionally constrained; surprise PRs that
   change protocol shapes will likely be rejected even if they are
   technically correct.
2. **Match the existing tone.** Specs are precise, terse, and avoid
   marketing language. Code follows idiomatic Go and TypeScript style.
3. **Sign your commits.** We require [Developer Certificate of Origin](https://developercertificate.org/)
   sign-off on every commit. Use `git commit -s`.
4. **Run the local checks before pushing:**
   ```
   make test
   ```
5. **Keep PRs focused.** One topic per PR. Mixed-topic PRs will be
   asked to split.

## Project scope

Nova is an umbrella project for content-addressed federated storage.
Product layers (`nova-image`, future `nova-video`, etc.) live in
designated subtrees of this repository. Adapters for external systems
(fediverse servers, forum software, CMS platforms) live in **separate
repositories** and consume only Nova's public HTTP API.

Please do **not** contribute integration code for specific external
systems to this repository. Those contributions belong in the
adapter repos.

## Commit message style

Conventional Commits prefix is recommended but not required:

```
feat(coordinator): add signed-URL HMAC verification
fix(node): retry pin assignment on transient network error
docs(specs): clarify CID encoding in encryption envelope
```

Subject line under 72 characters. Body wrapped at 72.

## Toolchain currency

Staying current is treated as routine maintenance, not incident response:

- **Node** tracks the active LTS line (`.nvmrc` is authoritative; CI uses
  `node-version-file`, the Docker node-builder pins the same major). When a
  Node LTS transition happens, the bump lands within the next milestone.
- **Go** — the `go.mod` directive tracks current stable Go; CI derives its
  version from `go.mod` (`go-version-file`), never a workflow literal.
- **golangci-lint** — the CI action installs the current major
  (`version: latest`); `.golangci.yml` pins `version: "2"` config schema, so
  a future linter major (v3) will fail loudly at config-load rather than
  drift silently. Bump the config schema deliberately when that happens.
- **Dependabot** (`.github/dependabot.yml`) watches gomod, npm, and
  github-actions weekly. Triage of alerts and grouped update PRs is part of
  every milestone's definition of done: determine reachability on Nova,
  record the verdict (see the M14 design's triage table for the format), and
  patch — exploitable or not — unless a bump is genuinely breaking.

## Versioning

Nova uses semantic versioning, and **every milestone is released under a
unique version** — no two builds of Nova, however small the change,
share a version number. Milestones are tagged annotated commits (see the
tag history and `docs/ROADMAP.md`); release artifacts derive their
version from `git describe --tags --always --dirty`, so any build off an
untagged or dirty tree is still uniquely identified. See
[`docs/VERSIONING.md`](docs/VERSIONING.md) for the full policy.

## Code of Conduct

This project follows the [Contributor Covenant](https://www.contributor-covenant.org/),
v2.1. See [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

By contributing, you agree that your contributions will be licensed
under the [Apache License 2.0](LICENSE).
