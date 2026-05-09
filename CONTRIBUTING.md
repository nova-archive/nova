# Contributing to Nova

Thanks for your interest in contributing. This project is in **Phase 0**
(specifications and contracts), so most contributions at this stage are
review and refinement of the documents under `docs/specs/` rather than
production code.

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

## Code of Conduct

This project follows the [Contributor Covenant](https://www.contributor-covenant.org/),
v2.1. See [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).

## License

By contributing, you agree that your contributions will be licensed
under the [Apache License 2.0](LICENSE).
