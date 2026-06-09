# syntax=docker/dockerfile:1

# ---- go-builder: coordinator + novactl + migrate (cgo libvips) ----
FROM golang:1.26-bookworm AS go-builder
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
FROM node:20-bookworm AS node-builder
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
FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips42 ca-certificates gosu && rm -rf /var/lib/apt/lists/*
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
