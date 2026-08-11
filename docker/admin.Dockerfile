# Nova admin tooling image (P2-M7.2, D-M7.2-1).
#
# Carries novactl plus a pinned nebula-cert. This is what makes the documented
# federation path require no Go toolchain and no nebula-cert on the operator's
# host — previously, "you do not need Go on the host" was true of the quickstart
# and false of federation bootstrap.
#
# Run-to-completion only:
#   docker compose --profile federation run --rm nova-admin federation init ...
#
# It is the ONLY container that mounts nova-fedpki. Nothing here is a daemon,
# so issuance authority is present only for the lifetime of one command.

# --- build novactl ---------------------------------------------------------
FROM golang:1.26-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# P2-M7.3 P0-c: one stamping surface across all three images. This previously
# set main.buildVersion, a symbol cmd/novactl does not define, so the flag was
# accepted and discarded and the shipped novactl reported "dev".
ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    BI=github.com/nova-archive/nova/internal/buildinfo; \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X $BI.version=${NOVA_VERSION} -X $BI.revision=${NOVA_REVISION} -X $BI.buildDate=${NOVA_BUILD_DATE}" \
      -o /out/novactl ./cmd/novactl

# --- pinned nebula-cert ----------------------------------------------------
# Taken from the same digest the donor bundle pins, so the operator issuing a
# certificate and the donor validating it agree on the tool version.
FROM nebulaoss/nebula:1.11.0@sha256:1bee6515faf687e590ab42e14a769d406bf1dc59cb03e21520ae50669adc0581 AS nebula

# --- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 10001 --create-home --home-dir /home/nova nova

COPY --from=build   /out/novactl  /usr/local/bin/novactl
# nebula-cert lives at the image ROOT in nebulaoss/nebula, not /usr/bin. The
# previous path made this stage fail outright; nothing caught it because no CI
# job built the admin image, and D-M7.3-3 makes it a released artifact.
COPY --from=nebula  /nebula-cert  /usr/local/bin/nebula-cert

# The PKI volume is admin-only; create the mount point with tight permissions.
RUN mkdir -p /var/lib/nova/fedpki /invites /import \
 && chown -R nova:nova /var/lib/nova /invites \
 && chmod 0700 /var/lib/nova/fedpki

ARG NOVA_VERSION=dev
ARG NOVA_REVISION=unknown
ARG NOVA_BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nova-admin" \
      org.opencontainers.image.description="Nova admin tooling: novactl plus a pinned nebula-cert" \
      org.opencontainers.image.version="${NOVA_VERSION}" \
      org.opencontainers.image.revision="${NOVA_REVISION}" \
      org.opencontainers.image.created="${NOVA_BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/nova-archive/nova" \
      org.opencontainers.image.licenses="Apache-2.0"

USER nova
WORKDIR /home/nova

ENTRYPOINT ["/usr/local/bin/novactl"]
CMD ["federation", "--help"]
