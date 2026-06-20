# Nova

**Networked Object Versatile Archive** — a self-hostable, federated,
content-addressed blob storage system for communities that need
sovereign, durable, and privacy-respecting hosting of large binary
objects.

Nova is an umbrella project. The first product layer, `nova-image`,
provides drag-and-drop image hosting with on-the-fly transforms.
Future product layers (`nova-video`, `nova-audio`, `nova-archive`,
`nova-document`) will share the same storage core.

> **Status:** Phase 1 (single-node MVP) is **complete** at
> `v0.1.0-rc1` — all fourteen milestones (foundation through setup
> wizard, Docker production, and release polish) are tagged. **Phase 2
> (federation + streaming-AEAD envelope) is in progress:** the
> operator-UX/privacy remediation track (P2-M0.1–M0.6) plus the first
> federation milestones — build/repo separation (P2-M1) and live
> identity/registration over mTLS (P2-M2) — are tagged; assignment
> synchronization (P2-M3) is next. New operators: start at
> [`docs/quickstart.md`](docs/quickstart.md). See
> [`docs/ROADMAP.md`](docs/ROADMAP.md) for per-milestone status.

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
3. **No third-party traffic intermediation by default.** A default
   Nova deployment serves all reads from the coordinator; donor
   nodes replicate over an encrypted mesh; no CDN is in the request
   path. Optional CDN fronting is documented as a deliberate
   tradeoff (CDN edges see plaintext); see `docs/recipes/CLOUDFLARE.md`.
4. **Framework-agnostic integration.** Any system that accepts an HTTP
   URL can integrate Nova by pointing URLs at it. No deep integration
   required.
5. **Privacy-paranoid by default.** No phone-home, no analytics, no
   third-party assets. A `paranoid: true` switch hardens further for
   adversarial environments.
6. **Permissive licensing.** Apache-2.0 throughout the core, with no
   copyleft dependencies.

> **Trust-model note.** Nova is donor-blind, not operator-blind.
> The coordinator decrypts content on every read and on transform;
> the operator's master key is process-resident. Nova is the right
> architecture for "pick an operator you trust, or run your own."
> It is not end-to-end encrypted from the operator. See
> `docs/THREAT_MODEL.md` for the full framing.

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
.github/        CI workflows, security policy, code owners
internal/       internal Go packages (subject to change)
pkg/            exported, semver-stable Go library packages
cmd/            command-line entry points
web/widget/     drop-in upload widget (TypeScript)
web/admin/      operator admin SPA (TypeScript)
nginx/          reference reverse-proxy configuration
```

## Project status

Phase 0 (specifications) and Phase 1 (single-node MVP) are complete;
Phase 1 closed at the `v0.1.0-rc1` release candidate (M1 through M14
are tagged). **Phase 2 (federation + streaming-AEAD envelope) is in
progress:** the P2-M0.x operator-UX/privacy remediation track and the
first two federation milestones (P2-M1 build/repo separation, P2-M2
identity/registration) are tagged; P2-M3 (assignment synchronization)
is next. See [`docs/ROADMAP.md`](docs/ROADMAP.md) for the authoritative
per-milestone status.

## Try Nova (developer setup)

> **Dev walkthrough.** This section boots a single-node coordinator
> against a local Postgres + embedded IPFS for kicking the tires. For
> a production-style first-run, follow the operator quickstart at
> [`docs/quickstart.md`](docs/quickstart.md): `docker compose
> --profile setup up` (in `docker/`), then open the loopback-only
> wizard at `http://127.0.0.1:8444/setup/` (or run the headless
> `novactl setup`). See
> [`docs/legal/OPERATOR_CHECKLIST.md`](docs/legal/OPERATOR_CHECKLIST.md)
> § "First-run setup (M13)" for the three first-run paths, TLS-mode
> guidance, and the secrets-backup obligation.

### Prerequisites

- Linux host (or WSL2). macOS works but `govips`/`libvips` host setup
  varies; on macOS install `libvips` via Homebrew before `go run`.
- **Go** 1.26 or newer (`go.mod` pins the toolchain).
- **Node** 22 (`.nvmrc` is authoritative) — only needed to build the
  SPAs/widget (`web/*`); the Go dev walkthrough below does not use it.
- **Docker** + `docker compose` plugin.
- `pkgconf`, `gcc`, `openssl`. The `govips` cgo build needs the first
  two; `openssl` is used here to generate dev keys.

On Arch Linux:

```sh
sudo pacman -S --needed go docker docker-compose pkgconf gcc openssl
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"   # log out + back in for the group to take
```

### 1. Bring up Postgres

```sh
git clone git@github.com:nova-archive/nova.git
cd nova
cp docker/.env.example docker/.env
sed -i "s/changeme/$(openssl rand -hex 16)/" docker/.env
docker compose -f docker/docker-compose.yml up -d postgres
```

### 2. Apply migrations

```sh
make migrate-up
```

This builds `cmd/migrate` and applies every migration through
`internal/db/migrations/`. `make migrate-status` shows current state;
`make smoke` runs the full schema-assertion smoke test.

### 3. Generate dev secrets

Nova needs three secret artifacts: a master key (envelope wrapping),
an Ed25519 signing key (local OIDC issuer), and an IPFS swarm key
(private mesh).

```sh
mkdir -p /tmp/nova-dev/kubo-repo /tmp/nova-dev/secrets
chmod 700 /tmp/nova-dev/secrets

# Master key: 32 random bytes, hex-encoded.
openssl rand -hex 32 > /tmp/nova-dev/secrets/master-key

# Local OIDC signing key: Ed25519 seed (32 random bytes, hex-encoded).
openssl rand -hex 32 > /tmp/nova-dev/secrets/oidc-signing-key

# IPFS private swarm key (Kubo PSK v1 format).
{ printf '/key/swarm/psk/1.0.0/\n/base16/\n'; openssl rand -hex 32; } \
    > /tmp/nova-dev/secrets/swarm.key

chmod 600 /tmp/nova-dev/secrets/*
```

### 4. Run the coordinator

```sh
set -a
source docker/.env
DATABASE_URL="postgres://nova:${POSTGRES_PASSWORD}@127.0.0.1:5432/nova?sslmode=disable"
NOVA_KUBO_REPO=/tmp/nova-dev/kubo-repo
IPFS_SWARM_KEY_FILE=/tmp/nova-dev/secrets/swarm.key
NOVA_MASTER_KEY_ACTIVE=v1
NOVA_MASTER_KEY_V1_FILE=/tmp/nova-dev/secrets/master-key
NOVA_OIDC_SIGNING_KEY_FILE=/tmp/nova-dev/secrets/oidc-signing-key
set +a

make run-coordinator
```

The coordinator listens on `:9000` by default (override with
`NOVA_LISTEN_ADDR`). See `cmd/coordinator/main.go` for the full
environment-variable table.

### 5. Smoke-test the read path

```sh
curl http://127.0.0.1:9000/health
curl http://127.0.0.1:9000/api/v1/auth/config
```

Both should return 200 with a JSON body. From here:

- **Anonymous endpoints** (`/health`, `/blob/{cid}`, `/blob/{cid}.json`,
  `/api/v1/auth/config`, `/api/v1/auth/jwks.json`) work without
  credentials.
- **Authenticated endpoints** (uploads at `/api/v1/uploads`,
  `/api/v1/blobs`, `/api/v1/images`, plus `/api/v1/users/me`) require
  a bearer token. The production setup wizard (M13) creates the first
  operator account; for this manual dev path, insert an `operator`
  user via `psql` with an argon2id password hash (see
  `internal/auth/password` for the format), then `go run
  ./cmd/novactl auth login` to fetch a token.

### Beyond this dev walkthrough

This manual recipe is the lightest dev-test path. Everything the
Phase 1 milestones promised is shipped and tagged (M1–M14):
signed-URL HMAC (M7), integrity-audit listing (M8), DMCA/moderation
(M9), master-key rotation (M10), the admin SPA (M11) and drag-and-drop
widget (M12), the first-run setup wizard + production Docker + TLS
modes (M13), and the operator quickstart + end-to-end CI smoke (M14).
For a production-style first-run, use the setup wizard (`docker
compose --profile setup up`; see the dev-walkthrough note above) and
the operator quickstart at [`docs/quickstart.md`](docs/quickstart.md).
Phase 2 (federation + streaming-AEAD) is in progress; see
[`docs/ROADMAP.md`](docs/ROADMAP.md).

## Development MCP servers

Phase 1 onwards, this project ships a `.claude/settings.local.json` that
configures a Postgres MCP server (`nova-dev-postgres`). When the dev
Postgres container is up (`docker compose -f docker/docker-compose.yml up
-d postgres`), Claude Code sessions with this project loaded can query
the dev database directly via MCP.

The MCP connection string reads `POSTGRES_PASSWORD` from your shell env;
set it from `docker/.env` before running Claude Code:

```sh
set -a; source docker/.env; set +a
```

If you do not want the MCP server, delete or comment out the
`mcpServers.nova-dev-postgres` entry in `.claude/settings.local.json`.
The MCP is dev-only; production deployments do not use it.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Project naming hygiene is
enforced via CI; please read the policy section before submitting a PR.

## Security

To report vulnerabilities, see [`SECURITY.md`](.github/SECURITY.md).

## License

Apache License 2.0. See [`LICENSE`](LICENSE).
