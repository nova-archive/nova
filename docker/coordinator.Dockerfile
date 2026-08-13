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
# P2-M7.3 P0-c: every binary in this image is stamped from the same three build
# args, so an image and the binaries inside it cannot disagree about what they
# are. Unstamped defaults are honest ("dev"/"unknown"), not empty.
ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
RUN BI=github.com/nova-archive/nova/internal/buildinfo; \
    LD="-s -w -X $BI.version=${NOVA_VERSION} -X $BI.revision=${NOVA_REVISION} -X $BI.buildDate=${NOVA_BUILD_DATE}"; \
    go build -trimpath -ldflags="$LD" -o /out/coordinator ./cmd/coordinator \
 && go build -trimpath -ldflags="$LD" -o /out/novactl     ./cmd/novactl \
 && go build -trimpath -ldflags="$LD" -o /out/migrate     ./cmd/migrate

# ---- node-builder: admin + widget + setup hermetic bundles ----
FROM node:22-bookworm@sha256:c601a46abb4d2ab80a9dc3da208d50d1122642d53f17a101926ace71e5a9bf1c AS node-builder
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
FROM debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241 AS runtime
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

# OCI labels carry the same identity as the stamped binaries. `docker inspect`
# is how an operator answers "what is actually running" without exec'ing into a
# container, and how the release lock's descriptors are cross-checked.
ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nova-coordinator" \
      org.opencontainers.image.description="Nova coordinator: read path, upload path, federation control plane" \
      org.opencontainers.image.version="${NOVA_VERSION}" \
      org.opencontainers.image.revision="${NOVA_REVISION}" \
      org.opencontainers.image.created="${NOVA_BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/nova-archive/nova" \
      org.opencontainers.image.licenses="Apache-2.0"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
