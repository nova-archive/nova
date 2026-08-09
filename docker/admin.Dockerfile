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
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.buildVersion=${VERSION}" \
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

COPY --from=build   /out/novactl        /usr/local/bin/novactl
COPY --from=nebula  /usr/bin/nebula-cert /usr/local/bin/nebula-cert

# The PKI volume is admin-only; create the mount point with tight permissions.
RUN mkdir -p /var/lib/nova/fedpki /invites /import \
 && chown -R nova:nova /var/lib/nova /invites \
 && chmod 0700 /var/lib/nova/fedpki

USER nova
WORKDIR /home/nova

ENTRYPOINT ["/usr/local/bin/novactl"]
CMD ["federation", "--help"]
