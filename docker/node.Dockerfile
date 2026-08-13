# syntax=docker/dockerfile:1

# Base images digest-pinned (P2-M7.1); resolved 2026-07-05. To bump: make docker-refresh-digests or docker buildx imagetools inspect.

# ---- build: pure-Go donor binary (no cgo, no libvips) ----
FROM golang:1.26-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
# P2-M7.3 P0-c: cmd/node previously had no version symbol at all, so a donor
# could not report what it was running and the census could not be correct.
ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
RUN BI=github.com/nova-archive/nova/internal/buildinfo; \
    go build -trimpath \
      -ldflags="-s -w -X $BI.version=${NOVA_VERSION} -X $BI.revision=${NOVA_REVISION} -X $BI.buildDate=${NOVA_BUILD_DATE}" \
      -o /out/nova-node ./cmd/node

# An empty directory to seed the storage mount point (see the runtime stage).
RUN mkdir -p /seed/storage

# ---- runtime: distroless static (ships CA certs + nonroot user; no shell, no curl) ----
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
COPY --from=build /out/nova-node /usr/local/bin/nova-node

# The storage mount point must exist IN THE IMAGE, owned by the runtime user
# (P2-M7.3, P0-b). Docker seeds a fresh named volume from whatever is at the
# mount path in the image; when the path does not exist it creates it root-owned
# instead, and nova-node's startup write-probe of storage_dir then fails with
# "permission denied" on every boot. distroless has no shell, so the directory
# is copied in from the build stage rather than created with RUN. 65532 is the
# numeric form of distroless `nonroot` — used literally so the COPY does not
# depend on a passwd lookup in the target image.
COPY --from=build --chown=65532:65532 /seed/storage /var/lib/nova-node/data

ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nova-node" \
      org.opencontainers.image.description="Nova donor pinning node" \
      org.opencontainers.image.version="${NOVA_VERSION}" \
      org.opencontainers.image.revision="${NOVA_REVISION}" \
      org.opencontainers.image.created="${NOVA_BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/nova-archive/nova" \
      org.opencontainers.image.licenses="Apache-2.0"

USER nonroot:nonroot
# The binary checks itself; the image needs no curl/wget.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD ["/usr/local/bin/nova-node", "--healthcheck", "--config", "/etc/nova/node.yaml"]
ENTRYPOINT ["/usr/local/bin/nova-node"]
CMD ["--config", "/etc/nova/node.yaml"]
