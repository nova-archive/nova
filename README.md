# Nova

**Networked Object Versatile Archive** — a self-hostable, federated,
content-addressed blob storage system for communities that need
sovereign, durable, and privacy-respecting hosting of large binary
objects.

Nova is an umbrella project. The first product layer, `nova-image`,
provides drag-and-drop image hosting with on-the-fly transforms.
Future product layers (`nova-video`, `nova-audio`, `nova-archive`,
`nova-document`) will share the same storage core.

> **Status:** Phase 0 — specifications and contracts.
> No production code has shipped yet. See [`docs/ROADMAP.md`](docs/ROADMAP.md).

## Who is this for?

- **Fediverse instances** (Mastodon, Pleroma, Misskey) that want to
  shift media storage off the homeserver onto a federated pool of
  donor-operated nodes.
- **FOSS forums and community sites** that want drag-and-drop image
  hosting without depending on a third-party host with unpredictable
  longevity.
- **Machine learning dataset hosts** distributing reproducible training
  corpora to researchers via content-addressed URLs.
- **Hardware preservation archives** keeping high-resolution scans of
  PCBs, schematics, and obsolete documentation accessible long after
  vendor sites disappear.
- **Software release mirrors** distributing build artifacts, container
  images, or signed packages with content-addressed integrity.
- **Personal homelabs** running a private federation of friend or
  family nodes for photo libraries, scanned documents, or backups.

## Design priorities

1. **Operator sovereignty.** You run the coordinator on your own
   infrastructure. The project author cannot turn it off, observe it,
   or coerce its behavior.
2. **Donor-blind storage.** Federated nodes pin opaque ciphertext, not
   plaintext. Encryption keys are held only by the coordinator.
3. **Framework-agnostic integration.** Any system that accepts an HTTP
   URL can integrate Nova by pointing URLs at it. No deep integration
   required.
4. **Privacy-paranoid by default.** No phone-home, no analytics, no
   third-party assets. A `paranoid: true` switch hardens further for
   adversarial environments.
5. **Permissive licensing.** Apache-2.0 throughout the core, with no
   copyleft dependencies.

## Architecture at a glance

A site **operator** runs a single coordinator process, which embeds an
IPFS daemon and exposes a simple HTTP API. Optional **federated
storage nodes**, run by donors, replicate ciphertext blobs over an
authenticated mesh and serve them on read.

```
   uploader / viewer
         │
         ▼
   nginx (TLS, rate-limit)
         │
         ▼
   Nova Coordinator ── Postgres
         │
         ├── embedded IPFS (hardened)
         └── mesh ──► donor storage nodes (×N)
```

Content is content-addressed: every blob is identified by the SHA-256
of its ciphertext. Reads use plain HTTPS URLs and are aggressively
CDN-cacheable.

## Repository layout

```
docs/
  specs/        protocol, data model, encryption envelope
  legal/        license, ToS template, DMCA procedure
  recipes/      deployment recipes (CDN, nginx, etc.)
.github/        CI workflows, issue templates, security policy
internal/       internal Go packages (subject to change)
pkg/            exported, semver-stable Go library packages
cmd/            command-line entry points
web/widget/     drop-in upload widget (TypeScript)
web/admin/      operator admin SPA (TypeScript)
nginx/          reference reverse-proxy configuration
```

## Phase 0 deliverables

See [`docs/ROADMAP.md`](docs/ROADMAP.md) for the full plan. Phase 0 is
specifications only — every subsequent phase implements them faithfully.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Project naming hygiene is
enforced via CI; please read the policy section before submitting a PR.

## Security

To report vulnerabilities, see [`SECURITY.md`](.github/SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
