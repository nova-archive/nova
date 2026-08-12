# Nova — developer setup

This is the lightest dev-test path: boot a single-node coordinator against a
local Postgres + embedded IPFS to kick the tires. It does **not** replace the
production first-run, which uses the setup wizard — see the
[operator quickstart](quickstart.md) and
[`legal/OPERATOR_CHECKLIST.md`](legal/OPERATOR_CHECKLIST.md) §"First-run setup"
for TLS-mode guidance and the secrets-backup obligation.

> Everything the Phase 1 milestones promised is shipped and tagged (M1–M14):
> signed-URL HMAC (M7), integrity-audit listing (M8), DMCA/moderation (M9),
> master-key rotation (M10), the admin SPA (M11) and drag-and-drop widget (M12),
> the first-run setup wizard + production Docker + TLS modes (M13), and the
> operator quickstart + end-to-end CI smoke (M14). Phase 2's donor federation is
> shipped through **P2-M7.1** — volunteers hosting a donor node start at
> [`quickstart/donor.md`](quickstart/donor.md).

## Prerequisites

- Linux host (or WSL2). macOS works but `govips`/`libvips` host setup varies;
  on macOS install `libvips` via Homebrew before `go run`.
- **Go** 1.26 or newer (`go.mod` pins the toolchain).
- **Node** 22 (`.nvmrc` is authoritative) — only needed to build the SPAs/widget
  (`web/*`); the Go dev walkthrough below does not use it.
- **Docker** + `docker compose` plugin.
- `pkgconf`, `gcc`, `openssl`. The `govips` cgo build needs the first two;
  `openssl` is used here to generate dev keys.

On Arch Linux:

```sh
sudo pacman -S --needed go docker docker-compose pkgconf gcc openssl
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"   # log out + back in for the group to take
```

## 1. Bring up Postgres

```sh
git clone git@github.com:nova-archive/nova.git
cd nova
cp docker/.env.example docker/.env
sed -i "s/changeme/$(openssl rand -hex 16)/" docker/.env
docker compose -f docker/docker-compose.yml -f docker/docker-compose.dev.yml up -d postgres
```

The **dev overlay is not optional** (P2-M7.3). `docker/docker-compose.yml` names
RELEASED artifacts by digest, from the release env an operator installs, and
carries no `build:` sections — a deployment should be able to say exactly which
bytes it runs, and with both a build section and an image ref the answer depends
on whether a stale local image happens to exist. `docker/docker-compose.dev.yml`
adds the builds back for people editing the source. `make` targets already pass
both.

## 2. Apply migrations

```sh
make migrate-up
```

This builds `cmd/migrate` and applies every migration through
`internal/db/migrations/`. `make migrate-status` shows current state;
`make smoke` runs the full schema-assertion smoke test.

## 3. Generate dev secrets

Nova needs three secret artifacts: a master key (envelope wrapping), an Ed25519
signing key (local OIDC issuer), and an IPFS swarm key (private mesh).

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

## 4. Run the coordinator

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

The coordinator listens on `:9000` by default (override with `NOVA_LISTEN_ADDR`).
See `cmd/coordinator/main.go` for the full environment-variable table.

## 5. Smoke-test the read path

```sh
curl http://127.0.0.1:9000/health
curl http://127.0.0.1:9000/api/v1/auth/config
```

Both should return 200 with a JSON body. From here:

- **Anonymous endpoints** (`/health`, `/blob/{cid}`, `/blob/{cid}.json`,
  `/api/v1/auth/config`, `/api/v1/auth/jwks.json`) work without credentials.
- **Authenticated endpoints** (uploads at `/api/v1/uploads`, `/api/v1/blobs`,
  `/api/v1/images`, plus `/api/v1/users/me`) require a bearer token. The
  production setup wizard (M13) creates the first operator account; for this
  manual dev path, insert an `operator` user via `psql` with an argon2id
  password hash (see `internal/auth/password` for the format), then
  `go run ./cmd/novactl auth login` to fetch a token.

## Testing & gates

The suite uses [testcontainers](https://testcontainers.com) — each DB-backed
test spins its own ephemeral Postgres, so `make test` needs Docker but not the
dev Postgres above.

| Command | What it checks |
|---------|----------------|
| `make test` | Full Go suite (~13 min). |
| `make web` | Builds + tests the `web/*` SPAs and widget (needs Node 22). |
| `make smoke` | Schema-assertion smoke test. |
| `make codegen-check` | sqlc output matches `internal/db/queries/*.sql` (never hand-edit `internal/db/gen/*`; run `make sqlc-generate`). |
| `make migrations-frozen` | Shipped migrations are append-only. |
| `make node-deps-check` | Donor dependency boundary (`github.com/prometheus/*` is hard-denied in the donor graph). |
| `make bench-corpus-explain` | Index-availability EXPLAIN gate. |

## Development MCP servers

This project ships a `.claude/settings.local.json` that configures a Postgres
MCP server (`nova-dev-postgres`). When the dev Postgres container is up, Claude
Code sessions with this project loaded can query the dev database directly via
MCP. The connection string reads `POSTGRES_PASSWORD` from your shell env; set it
from `docker/.env` before running Claude Code:

```sh
set -a; source docker/.env; set +a
```

To opt out, delete or comment the `mcpServers.nova-dev-postgres` entry. The MCP
is dev-only; production deployments do not use it.
