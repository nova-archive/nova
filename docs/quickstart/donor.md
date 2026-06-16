# Running a Nova donor node (`nova-node`)

> **Status: P2-M1 stub.** The full volunteer walkthrough — digest pinning,
> `cosign verify`, Nebula enrollment, and operational runbooks — lands in
> **P2-M7**. This page records only the release-trust model.

## Release trust

`nova-node` images are published to `ghcr.io/nova-archive/nova-node` and signed
with **cosign keyless (GitHub OIDC)**; each image carries an SBOM and a build
provenance attestation. There is **one** trust path: keyless signatures. Nova
does not publish a local-key signing path.

In production, **pin a digest, not a tag**, and verify the signature before
running a privileged network daemon (the exact `cosign verify` invocation +
identity/issuer policy is documented in the P2-M7 walkthrough):

    docker pull ghcr.io/nova-archive/nova-node@sha256:<digest>
    cosign verify ghcr.io/nova-archive/nova-node@sha256:<digest> ...

## What M1 ships

A minimal, dependency-boundary-enforced donor binary that loads + validates
`node.yaml` and serves a loopback health endpoint. **No federation yet** —
registration, transport, replication, healing, and audits arrive in M2–M7.
