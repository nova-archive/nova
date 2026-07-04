# Running a Nova donor node (`nova-node`)

> **Status: P2-M7 volunteer release.** This is the complete donor walkthrough:
> verify → enroll → run → confirm → upgrade → leave. The operational
> runbooks live in [`../runbooks/donor-lifecycle.md`](../runbooks/donor-lifecycle.md)
> and [`../runbooks/failure-drills.md`](../runbooks/failure-drills.md); network
> posture guidance is in
> [`../VOLUNTEER_DEPLOYMENT_GUIDANCE.md`](../VOLUNTEER_DEPLOYMENT_GUIDANCE.md).

A donor pins **opaque ciphertext** for a federation. You never hold keys, never
see plaintext, and expose no public ports. Everything below assumes your
federation operator has invited you and will issue your certificates.

## 1. Verify the image (before running anything)

`nova-node` images are published to `ghcr.io/nova-archive/nova-node`, pushed
**by digest** and signed with **cosign keyless (GitHub OIDC)**. There is one
trust path: keyless signatures from this repository's `ci.yml` on `main`.
Nova does not publish a local-key signing path.

**Pin a digest, not a tag.** Your operator's invite names the release digest.

```sh
DIGEST=sha256:<digest-from-your-operator>

# Signature: the signing identity is the ci.yml workflow on refs/heads/main
# of nova-archive/nova, via the GitHub Actions OIDC issuer. (Derived from
# .github/workflows/ci.yml `donor-sbom-sign`; if the workflow file moves,
# re-derive the identity from the workflow that signed your digest.)
cosign verify \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/nova-archive/nova/\.github/workflows/ci\.yml@refs/heads/main$' \
  ghcr.io/nova-archive/nova-node@$DIGEST

# SBOM attestation (SPDX JSON, attested to the SAME digest):
cosign verify-attestation --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/nova-archive/nova/\.github/workflows/ci\.yml@refs/heads/main$' \
  ghcr.io/nova-archive/nova-node@$DIGEST

# Build provenance (GitHub attestation; needs the gh CLI):
gh attestation verify oci://ghcr.io/nova-archive/nova-node@$DIGEST \
  --repo nova-archive/nova
```

All three must verify. If any fails, stop and contact your operator — do not
run the image.

## 2. Enroll

Your **operator** issues your identity; you never generate federation
certificates yourself.

The operator runs:

```sh
novactl node issue --dir <ca-dir> --name <your-name> --out <bundle-dir>
```

and sends you the bundle: `federation.crt`, `federation.key`,
`federation-ca.crt`, and `node-manifest.json` (your `node_id` and cert
fingerprint — keep it; it is how you identify yourself in support requests).

Nebula overlay enrollment (lighthouse address, `nebula-cert` signing, and the
port/firewall posture) follows
[`../VOLUNTEER_DEPLOYMENT_GUIDANCE.md`](../VOLUNTEER_DEPLOYMENT_GUIDANCE.md).
The operator can generate a config skeleton for you:

```sh
novactl node nebula-template --name <your-name> --nebula-ip 10.42.0.NN/24 --out <dir>
```

That template emits an annotated `node.yaml`. The fields that matter:

```yaml
coordinator_url: "https://<coordinator-overlay-ip>:9443"   # the FEDERATION listener
federation_ca_path:   /etc/nova/federation/federation-ca.crt
federation_cert_path: /etc/nova/federation/federation.crt
federation_key_path:  /etc/nova/federation/federation.key
storage_dir: /var/lib/nova-node
bandwidth_budget_bytes_per_day: 53687091200   # 50 GiB/day cap — your knob
kubo_api_addr: "http://127.0.0.1:5001"        # the loopback Kubo sidecar
source_nebula_addr: "<your-overlay-ip>:9555"  # advertised read-source address
source_read_listen_addr: "0.0.0.0:9555"       # bind for the read-source mTLS listener
```

## 3. Run

Use the compose file from the template (`compose.yaml`): a Nebula sidecar plus
`nova-node` sharing its network namespace, no published ports. Pin the digest
you verified in step 1:

```yaml
  nova-node:
    image: ghcr.io/nova-archive/nova-node@sha256:<digest>
```

```sh
docker compose up -d
docker compose ps    # nova-node healthcheck: `--healthcheck --config ...`
```

Two first-boot behaviors are **normal** (not failures):

- **Initial cadence:** a fresh donor heartbeats every 300 s and polls for pin
  work every 600 s until its FIRST heartbeat delivers the federation's real
  timers. Expect up to ~10 minutes before the first assignments flow.
- **Read-source starts on the second boot:** the donor's read-source listener
  binds only when a persisted registration already exists at boot
  (fail-closed identity binding). Restart the container once after your first
  successful registration; the log line `nova-node: read-source on ...`
  confirms it.

## 4. Confirm you are serving

Ask your operator to run:

```sh
novactl node list
```

Your node should show `active` with `LAST_SEEN` moving. On the operator's
`/metrics` plane, `nova_nodes{status="active",...}` includes you, and
possession audits (`nova_audit_results_total{result="pass"}`) begin within the
audit cadence — passing audits are the proof you are truly storing and serving
your assigned bytes.

## 5. Upgrade / rollback

Upgrades are a digest re-pin: verify the NEW digest (step 1), edit the compose
image line, `docker compose up -d`. Rollback is the same operation with the
previous digest.

Mixed versions are supported across one milestone: an N−1 donor interoperates
with the current coordinator (and vice versa) — join, replication, and audits
are covered by the release's cross-version gate (`make crossversion-e2e`,
D-M7-3). Don't run further behind than one milestone.

## 6. Leaving gracefully

Tell your operator you want to leave; they run:

```sh
novactl node drain --id <your-node-id>
```

Draining means: no new placements, your replicas stop counting toward
durability, but your node **keeps serving** as a repair source while its data
is re-replicated elsewhere.

**Keep the donor RUNNING until the operator confirms zero drain debt**
(`nova_node_drain_pending_cids{node_id=...} == 0`). Shutting down early makes
the federation heal from scratch instead of from you. Once the operator
confirms and revokes your node, you can `docker compose down -v` and delete
the data directory. Full procedure (including the mistaken-drain `undrain`
path): [`../runbooks/donor-lifecycle.md`](../runbooks/donor-lifecycle.md).
