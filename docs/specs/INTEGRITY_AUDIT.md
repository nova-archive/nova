# Integrity Audit

Status: **Phase 1 deliverable, normative.** `internal/audit/integrity`
must conform exactly.

## Purpose

Local, coordinator-internal correctness checks that catch
implementation bugs and silent corruption before donors are involved.
This is **not** a donor-facing audit — there are no donor messages,
no challenge tokens, no remote calls. It runs entirely against the
coordinator's own database, local Kubo blockstore, and local
encryption keys.

The integrity audit is the Phase 1 proof-of-correctness backbone.
Its presence (and its successful pass rate) is what gives an
operator confidence that the storage core is functioning correctly
before they expose donors to their data.

## Scope

The audit verifies seven invariants that, together, prove the
coordinator's local state is consistent. Each is implemented as a
distinct `audit_kind` enum value (matching the SQL enum in
`DATA_MODEL.sql`).

| `audit_kind` | What it checks |
|---|---|
| `envelope_decode` | Sampled blob bytes parse as a valid envelope (magic, version, algo, reserved zero, nonce length) |
| `key_unwrap` | The corresponding `data_encryption_keys` row's wrapped_key unwraps with the recorded master-key version |
| `sample_decrypt` | A small random byte range of the decrypted plaintext yields a valid AEAD authentication tag |
| `kubo_pin_present` | The local Kubo daemon reports the CID as pinned |
| `derivative_state_consistent` | A sampled derivative blob's state matches its parent's state (e.g., parent quarantined ⇒ derivative quarantined) |
| `block_hash_valid` | Recorded `blob_blocks.block_cid` values, when re-fetched and re-hashed, match the stored CID |
| `manifest_consistent` | `blob_manifests` row's `block_count` matches the count of associated `blob_blocks` rows; `envelope_size` matches sum of `block_size` |

## Schedule

Each `audit_kind` runs on its own cadence:

| `audit_kind` | Default interval | Sample size |
|---|---|---|
| `envelope_decode` | hourly | 100 random blobs |
| `key_unwrap` | hourly | 100 random keys |
| `sample_decrypt` | hourly | 50 random blobs |
| `kubo_pin_present` | every 15 min | 200 random blobs |
| `derivative_state_consistent` | hourly | every derivative whose parent state changed in the past hour |
| `block_hash_valid` | daily | 100 random blocks (multi-block blobs) |
| `manifest_consistent` | daily | 100 random blobs |

Operator-tunable in `operator.yaml` under `integrity_audit`. Setting
any interval to 0 disables that audit kind (dev-mode only;
production builds refuse). Setting sample sizes is permitted within
sane bounds (1..10000).

## Reporting

Every audit run inserts a row into `integrity_audits`:

```sql
INSERT INTO integrity_audits (cid, audit_kind, result, error)
VALUES (...);
```

Failures (`result = 'fail'`) are also:

- Counted in the metric
  `nova_integrity_audit_failures_total{audit_kind=...}`.
- Logged at warn level with the affected CID, audit kind, and
  error detail.
- Optionally emitted via the `integrity.audit_failed` outbound
  webhook (operator-configured; off by default).

The admin UI surfaces a "recent failures" panel pulling from
`integrity_audits WHERE result <> 'pass' ORDER BY audited_at DESC`.

## Failure handling

The audit reports failures; it does not auto-remediate. Decisions
about what to do with a failed audit are operator policy:

| Failure | Likely cause | Operator action |
|---|---|---|
| `envelope_decode` fail | Bytes corrupted in Kubo blockstore, or implementation bug | Re-fetch from a donor (if any have it); investigate the local blockstore |
| `key_unwrap` fail | Master-key mismatch (rotation in progress, wrong env var) or DB corruption | Verify NOVA_MASTER_KEY versions; restore from backup if corruption |
| `sample_decrypt` fail | Tampered ciphertext or key/envelope mismatch | Same as envelope_decode; investigate |
| `kubo_pin_present` fail | Local Kubo lost the pin | Re-pin from blob_blocks list (which records every block) or fetch from donors |
| `derivative_state_consistent` fail | Bug in the cascade in product OnDelete | Manually cascade the parent's state to derivatives |
| `block_hash_valid` fail | Single-block corruption; rare | Re-fetch the block from donors |
| `manifest_consistent` fail | Implementation bug or partial-write recovery edge case | Investigate; possibly rebuild the manifest from the live envelope |

For each failure mode, the admin UI offers a one-click "remediate"
action that runs the standard fix where one exists. Where no
automated fix is appropriate (e.g., manifest corruption), the UI
flags the case for operator review.

## Performance considerations

The audit runs as a background goroutine in the coordinator
process. Per-run cost:

- `envelope_decode`: ~1 ms per blob (header parse only).
- `key_unwrap`: ~10 µs per key (one AEAD decrypt of 32 bytes).
- `sample_decrypt`: ~5 ms per blob (decrypt sampled bytes).
- `kubo_pin_present`: ~1 ms per blob (Kubo API call, batched).
- `block_hash_valid`: ~10 ms per block (re-fetch + sha256).

A 1 M-blob deployment running default schedules generates about
500 audit rows per minute, sampled across the corpus. The runtime
overhead is negligible (~0.1 % CPU on commodity hardware).

The `integrity_audits` table grows linearly with audit volume.
Operators retain failures indefinitely for forensic value; passes
are pruned by a scheduled job to a configurable retention window
(default 30 days for passes; failures retained ≥ 1 year).

## Restart behaviour

Audits resume on coordinator restart from the schedule's natural
cadence. There is no persistent in-flight queue.

## Cross-references

- Schema: `docs/specs/DATA_MODEL.sql` (`integrity_audits` table,
  `audit_kind` enum, `audit_result` enum).
- Encryption: `docs/specs/ENCRYPTION_ENVELOPE.md` (envelope format,
  key unwrap semantics).
- IPFS layout: `docs/specs/IPFS_IMPORT_RULES.md` (deterministic CID
  rules used by `block_hash_valid`).
- Phase 2 donor audits: `docs/specs/POSSESSION_AUDIT.md`.
