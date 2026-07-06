# syntax=docker/dockerfile:1

# Base images digest-pinned (P2-M7.1); resolved 2026-07-05. To bump: make docker-refresh-digests or docker buildx imagetools inspect.

# ---- go-builder: coordinator + novactl + migrate (cgo libvips) ----
FROM golang:1.26-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS go-builder
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips-dev pkg-config gcc && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/coordinator ./cmd/coordinator \
 && go build -trimpath -ldflags="-s -w" -o /out/novactl     ./cmd/novactl \
 && go build -trimpath -ldflags="-s -w" -o /out/migrate     ./cmd/migrate

# ---- node-builder: admin + widget + setup hermetic bundles ----
FROM node:26-bookworm@sha256:35d3b83382381e0e2f1d066b98aba486a4fab481a241c7516389635b88d927c1 AS node-builder
WORKDIR /src
COPY package.json package-lock.json ./
COPY web/admin/package.json  web/admin/package.json
COPY web/widget/package.json web/widget/package.json
COPY web/setup/package.json  web/setup/package.json
RUN npm ci
COPY . .
RUN npm run -w @nova/admin build \
 && npm run -w @nova/widget build \
 && npm run -w @nova/setup build

# ---- runtime: Debian-slim/glibc; entrypoint drops to non-root via gosu ----
# govips/libvips requires glibc — distroless/alpine will not link.
# The entrypoint runs as root, chowns mounted volumes to the nova user,
# then drops privileges via gosu for migrate + the final coordinator exec.
FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS runtime
# curl exists solely for the compose healthcheck probe of /health — the
# image ships no other HTTP client (no wget, no busybox).
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips42 ca-certificates gosu curl && rm -rf /var/lib/apt/lists/*
RUN groupadd -r nova && useradd -r -g nova -u 10001 nova
COPY --from=go-builder /out/coordinator /out/novactl /out/migrate /usr/local/bin/
COPY --from=node-builder /src/web/admin/dist  /usr/share/nova/admin
COPY --from=node-builder /src/web/widget/dist /usr/share/nova/widget
COPY --from=node-builder /src/web/setup/dist  /usr/share/nova/setup
COPY docker/init/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENV NOVA_ADMIN_DIST_DIR=/usr/share/nova/admin \
    NOVA_WIDGET_DIST_DIR=/usr/share/nova/widget \
    NOVA_SETUP_DIST_DIR=/usr/share/nova/setup
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
