# syntax=docker/dockerfile:1

# Base images digest-pinned (P2-M7.1); resolved 2026-07-05. To bump: make docker-refresh-digests or docker buildx imagetools inspect.

# ---- build: pure-Go donor binary (no cgo, no libvips) ----
FROM golang:1.26-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/nova-node ./cmd/node

# ---- runtime: distroless static (ships CA certs + nonroot user; no shell, no curl) ----
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/nova-node /usr/local/bin/nova-node
USER nonroot:nonroot
# The binary checks itself; the image needs no curl/wget.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD ["/usr/local/bin/nova-node", "--healthcheck", "--config", "/etc/nova/node.yaml"]
ENTRYPOINT ["/usr/local/bin/nova-node"]
CMD ["--config", "/etc/nova/node.yaml"]
