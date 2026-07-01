-- ============================================================================
-- Nova: Networked Object Versatile Archive
-- Data Model — Phase 0 v2 (Postgres 16)
--
-- This file is the Phase-0 v2 schema baseline. From Phase 1 onward, the
-- authoritative, evolving schema lives in internal/db/migrations/ (goose),
-- which is what sqlc reads (internal/db/sqlc.yaml: schema: "migrations") to
-- generate the Go query layer. Phase-1 migrations 0002+ (jobs, partitions,
-- envelope_version, upload_sessions, auth) are intentionally NOT backfilled
-- here; consult internal/db/migrations/ for the live schema.
--
-- v2 revisions vs the original Phase 0:
--   - keys → data_encryption_keys + signing_keys + master_key_versions
--     (purpose split, master-key rotation enabled in Phase 1)
--   - derivatives are now first-class blobs rows with parent_cid,
--     derivative_preset, derivative_format columns; the standalone
--     derivatives table is removed
--   - collections gain public_archival flag (encryption-opt-out gate)
--   - node_status enum split into 5 states with semantically distinct
--     timers (active, suspect, unreachable, evicted, revoked)
--   - signed_url_revocations restructured to (kind, value) tuples
--     (the old prefix-string scheme could not represent the documented
--     cid:/aud:/kid: forms because the canonical string starts with
--     the URL path)
--   - data_encryption_keys.legal_hold gates crypto-shred so DMCA
--     quarantine and severe-content preservation can hold keys
--   - moderation_decisions gains scheduled_tombstone_at and legal_hold
--     to support DMCA quarantine-first flow
--   - blob_manifests + blob_blocks added for proof-readiness (Phase 2
--     possession audits and Phase 8+ formal POR/PDP)
--   - integrity_audits + pin_audits + node reputation added for
--     Phase 1 local fixity and Phase 2 donor spot-checks
--
-- Phase 2 (P2-M0, 2026-06-13) schema deltas appear below as COMMENTARY only
-- (search "P2-M0"); the executable DDL ships as a new, non-frozen Phase 2
-- migration in P2-M3. No Phase 1 migration is modified (migrations-frozen gate
-- stays green). See
-- docs/superpowers/specs/phase2/2026-06-13-phase2-m0-spec-reconciliation-design.md.
--
-- Conventions:
--   - All timestamps are timestamptz, defaulted to now() where appropriate.
--   - All UUIDs are gen_random_uuid() unless explicitly user-supplied.
--   - All foreign keys cascade on delete only when the child has no
--     independent meaning (e.g., collection_items follows the collection;
--     audit_log does NOT cascade, because audit history must outlive the
--     thing it audits).
--   - Indexes are created concurrently in production migrations; declared
--     in-line here for clarity.
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;  -- for gen_random_uuid()

-- ----------------------------------------------------------------------------
-- Enums
-- ----------------------------------------------------------------------------

CREATE TYPE user_role AS ENUM (
    'viewer',     -- can read public content only
    'uploader',   -- can upload and manage their own content
    'moderator',  -- can review moderation queue and execute takedowns
    'operator'    -- full administrative access
);

CREATE TYPE blob_state AS ENUM (
    'active',         -- normal
    'soft_deleted',   -- user requested deletion; bytes still recoverable for grace window
    'quarantined',    -- moderation hold; reads return 451; key may or may not have legal_hold
    'tombstoned'      -- key shredded; bytes are unrecoverable ciphertext
);

CREATE TYPE blob_product AS ENUM (
    'image',
    'video',
    'audio',
    'archive',
    'document',
    'raw'
);

CREATE TYPE collection_visibility AS ENUM (
    'public',     -- listed and readable without authentication
    'unlisted',   -- readable with the slug; not listed
    'private'     -- requires owner credentials or signed URL
);

-- Five-state donor liveness model (v2). The original two-tier
-- (active/degraded/offline/revoked) collapsed "missed heartbeat" with
-- "long-term gone" and would have delayed mass-casualty healing for up
-- to max_offline_window. The five states separate the timers:
--   active      - heartbeating within tolerance
--   suspect     - missed 2-3 heartbeats (~15 min); still counted to avoid flapping
--   unreachable - missed > liveness SLA (~1 h); excluded from acked_count, healing engages
--   evicted     - exceeded max_offline_window (~30 d); pin_assignments removed
--   revoked     - operator marked compromised; cert revoked, immediate re-replication
CREATE TYPE node_status AS ENUM (
    'active',
    'suspect',
    'unreachable',
    'evicted',
    'revoked'
);

CREATE TYPE pin_state AS ENUM (
    'pending',    -- assignment dispatched, waiting for ack
    'acked',      -- node confirmed it has the bytes
    'failed',     -- node refused or could not retrieve
    'unpinning'   -- broadcast unpin sent; awaiting confirmation
);

CREATE TYPE moderation_rule AS ENUM (
    'pdq_match',        -- PDQ similarity match against blocklist
    'dmca',             -- DMCA takedown notice
    'severe_content',   -- CSAM-class severe-content rule (see SEVERE_CONTENT_PROCEDURE.md)
    'operator_manual',  -- operator decision without scanner
    'user_report'       -- end-user reported the content
);

CREATE TYPE moderation_action AS ENUM (
    'quarantine',  -- block reads but retain bytes pending review (default for DMCA)
    'tombstone',   -- shred encryption key; broadcast unpin (final)
    'allow'        -- explicitly clear (e.g., false-positive review)
);

CREATE TYPE dmca_status AS ENUM (
    'received',
    'investigating',
    'actioned',
    'rejected'
);

-- Key state values (v2). 'rotating' marks the in-progress SOURCE master_key_versions
-- row during a master-key rotation (M10: internal/masterkey). Individual DEK rows do
-- NOT get a per-row 'rotating' state; their state stays 'active' throughout. The
-- rotation worker's per-row update is one atomic, version-guarded UPDATE that flips
-- wrapped_key + master_key_version_id together (WHERE master_key_version_id = old).
-- For signing keys (signed URLs): 'retired' = rotated out but STILL VERIFIES
-- until retire_after (the grace window); 'shredded' = past grace, wrapped_key
-- zeroed. See docs/specs/SIGNED_URL_FORMAT.md "Key rotation".
CREATE TYPE key_state AS ENUM (
    'active',
    'rotating',
    'shredded',    -- past grace / crypto-shredded; wrapped_key zeroed
    'retired'      -- rotated out; signing keys still verify until retire_after
);

CREATE TYPE audit_kind AS ENUM (
    'envelope_decode',           -- envelope header parses, magic/version OK
    'key_unwrap',                -- per-blob key unwraps with master key
    'sample_decrypt',            -- random-byte sample decrypts and verifies tag
    'kubo_pin_present',          -- Kubo confirms local pin exists
    'derivative_state_consistent', -- derivative state matches parent
    'block_hash_valid',          -- recorded block_cid hashes match recomputed
    'manifest_consistent'        -- blob_manifests + blob_blocks match envelope
);

CREATE TYPE audit_result AS ENUM ('pass', 'fail', 'skip');

-- ----------------------------------------------------------------------------
-- Users
-- ----------------------------------------------------------------------------

CREATE TABLE users (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email       citext UNIQUE NOT NULL,
    role        user_role NOT NULL DEFAULT 'viewer',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- ----------------------------------------------------------------------------
-- Master key versions (v2)
--
-- Tracks the operator master key (NOVA_MASTER_KEY) over its rotation
-- history. Each per-blob and per-signing key row references the
-- master-key version it was wrapped under. Rotation (M10: internal/masterkey)
-- sets the source version's state to 'rotating', re-wraps every referenced
-- wrapped_key under the new master key via atomic guarded UPDATEs, then
-- marks the source version 'retired'. A version row is NEVER deleted — a
-- 'shredded' DEK keeps its FK to it forever — only 'retired'. The 'rotating'
-- state therefore marks the in-progress source version, not individual DEK rows.
--
-- The actual master-key bytes are never stored in this table. They live
-- in process memory loaded from NOVA_MASTER_KEY.
-- ----------------------------------------------------------------------------

CREATE TABLE master_key_versions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version_label   text UNIQUE NOT NULL,    -- 'v1', 'v2', '2026-Q2', etc.
    state           key_state NOT NULL DEFAULT 'active',
    created_at      timestamptz NOT NULL DEFAULT now(),
    retired_at      timestamptz
);

CREATE INDEX master_key_versions_state_idx ON master_key_versions (state);

-- ----------------------------------------------------------------------------
-- Per-blob data encryption keys (v2)
--
-- Replaces the original keys table for blob encryption. Keys are
-- wrapped (encrypted) with the operator master key (loaded from
-- environment) and persisted as bytea. The master_key_version_id
-- foreign key records which master-key version wrapped this row, so
-- the M10 re-wrap worker can find rows that need migrating.
--
-- dek_master_version_idx (master_key_version_id) WHERE state IN ('active','rotating')
-- is the M10 re-wrap worker's claim index (designed ahead for M10).
-- The worker uses FOR UPDATE SKIP LOCKED on this index to claim batches,
-- then performs one atomic, version-guarded UPDATE per row (flipping
-- wrapped_key + master_key_version_id together; no per-row 'rotating' state
-- is used). legal_hold rows are re-wrapped normally — re-wrap is not a shred
-- and the no_shred_under_legal_hold CHECK remains the floor.
--
-- legal_hold = true prevents crypto-shredding (the wrapped_key is not
-- zeroed and state remains 'active') regardless of blob.state. This is
-- required for severe-content preservation flows where evidence must be
-- retained under statutory obligation.
--
-- Crypto-shred procedure:
--   1. Verify legal_hold = false. If true, refuse and audit-log.
--   2. UPDATE state='shredded', shredded_at=now(), wrapped_key=zeroes(72).
--   3. Postgres autovacuum reclaims the old bytes within minutes.
-- ----------------------------------------------------------------------------

CREATE TABLE data_encryption_keys (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    algorithm                text NOT NULL,            -- 'XChaCha20-Poly1305'
    wrapped_key              bytea NOT NULL,           -- 72 bytes when active; zeroed on shred
    master_key_version_id    uuid NOT NULL REFERENCES master_key_versions (id),
    legal_hold               boolean NOT NULL DEFAULT false,
    state                    key_state NOT NULL DEFAULT 'active',
    created_at               timestamptz NOT NULL DEFAULT now(),
    shredded_at              timestamptz,

    CONSTRAINT no_shred_under_legal_hold CHECK (
        legal_hold = false OR state IN ('active', 'rotating')
    )
);

CREATE INDEX dek_state_idx ON data_encryption_keys (state);
CREATE INDEX dek_master_version_idx ON data_encryption_keys (master_key_version_id) WHERE state IN ('active', 'rotating');
CREATE INDEX dek_legal_hold_idx ON data_encryption_keys (legal_hold) WHERE legal_hold = true;

-- ----------------------------------------------------------------------------
-- Signing keys (v2)
--
-- HMAC-SHA256 keys for signed URLs. Lifecycle is independent of
-- per-blob keys: rotated on a schedule (default 90 days) with a grace
-- window during which both old and new keys verify. The kid field is
-- the public identifier embedded in signed URLs.
--
-- Like data_encryption_keys, wrapped_key is encrypted under the operator
-- master key (master_key_version_id records which version). Master-key
-- rotation (M10: internal/masterkey) MUST therefore re-wrap signing_keys rows
-- in state 'active' AND non-shredded 'retired' (both still hold real bytes
-- and still verify signed URLs), not just the blob DEKs — otherwise every
-- signing key is orphaned on rotation. 'shredded' rows are skipped (wrapped_key
-- already zeroed). See ENCRYPTION_ENVELOPE.md "Rotation procedure". (M7.)
-- ----------------------------------------------------------------------------

CREATE TABLE signing_keys (
    kid                     text PRIMARY KEY,           -- public identifier in signed URLs
    algorithm               text NOT NULL,              -- 'HMAC-SHA256'
    wrapped_key             bytea NOT NULL,
    master_key_version_id   uuid NOT NULL REFERENCES master_key_versions (id),
    state                   key_state NOT NULL DEFAULT 'active',
    active_from             timestamptz NOT NULL DEFAULT now(),
    retire_after            timestamptz,                -- grace window end; null until rotation begins
    created_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX signing_keys_state_idx ON signing_keys (state);

-- ----------------------------------------------------------------------------
-- Collections (v2: + public_archival)
-- ----------------------------------------------------------------------------

CREATE TABLE collections (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES users (id),
    name            text NOT NULL,
    slug            text NOT NULL,
    visibility      collection_visibility NOT NULL DEFAULT 'private',
    public_archival boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),

    UNIQUE (owner_id, slug),
    -- public_archival opts the collection out of envelope encryption.
    -- Only meaningful for genuinely public content; refused otherwise
    -- to prevent accidental privacy regressions.
    CONSTRAINT public_archival_requires_public_visibility
        CHECK (public_archival = false OR visibility = 'public')
);

CREATE INDEX collections_visibility_idx
    ON collections (visibility)
    WHERE visibility <> 'private';

-- ----------------------------------------------------------------------------
-- Blobs (v2: + parent_cid for derivatives, + product hooks)
--
-- Originals have parent_cid=NULL; derivatives reference the parent CID
-- and carry the (preset, format) tuple that produced them. Derivatives
-- are full first-class blobs: their own encryption_key_id, their own
-- state, their own pin_assignments. The standalone derivatives table
-- from v1 is gone; the lookup (parent, preset, format) -> derivative_cid
-- is served by the unique index below.
--
-- State cascade: when a parent transitions to quarantined or tombstoned,
-- the application layer also transitions all child derivatives. The
-- product module's OnDelete hook handles this.
-- ----------------------------------------------------------------------------

CREATE TABLE blobs (
    cid                   text PRIMARY KEY,
    encryption_key_id     uuid REFERENCES data_encryption_keys (id),  -- NULL only when parent collection.public_archival = true
    parent_cid            text REFERENCES blobs (cid),                 -- NULL for originals; non-NULL for derivatives
    derivative_preset     text,                                        -- e.g., 'thumb', 'w512', or NULL for originals
    derivative_format     text,                                        -- e.g., 'webp', 'jpeg', or NULL for originals
    owner_id              uuid REFERENCES users (id),
    mime_type             text NOT NULL,
    byte_size             bigint NOT NULL CHECK (byte_size >= 0),
    uploaded_at           timestamptz NOT NULL DEFAULT now(),
    state                 blob_state NOT NULL DEFAULT 'active',
    soft_deleted_at       timestamptz,                                 -- set on owner soft-delete; the lifecycle sweep ages it against NOVA_SOFT_DELETE_GRACE_SECONDS, then tombstones via the shared lifecycle.TombstoneTree primitive (the same crypto-shred path as moderation)
    source_ip             inet,
    product               blob_product NOT NULL DEFAULT 'raw',

    CONSTRAINT derivative_columns_consistent CHECK (
        (parent_cid IS NULL     AND derivative_preset IS NULL     AND derivative_format IS NULL) OR
        (parent_cid IS NOT NULL AND derivative_preset IS NOT NULL AND derivative_format IS NOT NULL)
    )
);

CREATE INDEX blobs_owner_state_idx
    ON blobs (owner_id, state)
    WHERE state <> 'tombstoned';

CREATE INDEX blobs_uploaded_at_idx ON blobs (uploaded_at);
CREATE INDEX blobs_product_state_idx ON blobs (product, state);
CREATE INDEX blobs_parent_cid_idx ON blobs (parent_cid) WHERE parent_cid IS NOT NULL;

-- Owner soft-delete sweep (M11): the in-process lifecycle sweep claims overdue
-- soft-deletes by ageing soft_deleted_at against the configured grace window.
CREATE INDEX blobs_soft_delete_sweep_idx
    ON blobs (soft_deleted_at)
    WHERE state = 'soft_deleted';

-- Lookup index for derivatives. Replaces the v1 derivatives table's
-- UNIQUE(parent_cid, preset, format) constraint.
CREATE UNIQUE INDEX blobs_derivative_lookup_idx
    ON blobs (parent_cid, derivative_preset, derivative_format)
    WHERE parent_cid IS NOT NULL;

-- ----------------------------------------------------------------------------
-- Block manifests (v2 — proof-readiness)
--
-- For each blob, records the deterministic IPFS import parameters and
-- the resulting block layout. See IPFS_IMPORT_RULES.md. Phase 1
-- consumers: integrity audits. Phase 2 consumers: donor possession
-- spot-checks. Phase 6+ consumers: formal POR/PDP, erasure coding,
-- transparency-log proofs.
-- ----------------------------------------------------------------------------

CREATE TABLE blob_manifests (
    cid                text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
    cid_version        smallint NOT NULL DEFAULT 1,
    hash_alg           text NOT NULL,                  -- 'sha2-256'
    codec              text NOT NULL,                  -- 'raw' for single-block, 'dag-pb' for UnixFS
    chunker            text NOT NULL,                  -- 'size-262144' (256 KiB fixed) for v1
    plaintext_size     bigint NOT NULL CHECK (plaintext_size >= 0),
    envelope_size      bigint NOT NULL CHECK (envelope_size >= 0),
    block_count        integer NOT NULL CHECK (block_count > 0),
    merkle_root        text,                           -- root CID for multi-block; NULL for single-block (== blob cid)
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE blob_blocks (
    blob_cid       text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    block_cid      text NOT NULL,
    block_index    integer NOT NULL CHECK (block_index >= 0),
    block_size     integer NOT NULL CHECK (block_size > 0),

    PRIMARY KEY (blob_cid, block_index)
);

CREATE INDEX blob_blocks_block_cid_idx ON blob_blocks (block_cid);

-- ----------------------------------------------------------------------------
-- Image-specific metadata (nova-image product layer)
--
-- One row per image-typed blob (originals AND derivatives). Derivatives
-- have NULL perceptual_hash because the parent's hash is the canonical
-- visual identity; deduplication and moderation operate on parents.
-- ----------------------------------------------------------------------------

CREATE TABLE image_metadata (
    cid              text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
    width            int NOT NULL CHECK (width > 0),
    height           int NOT NULL CHECK (height > 0),
    perceptual_hash  bytea,                            -- 32 bytes; Phase 3 Go-native 256-bit pHash (dedup), Phase 4 PDQ (external matching); NULL until then
    alt_text         text,
    caption          text
);

CREATE INDEX image_metadata_perceptual_hash_idx
    ON image_metadata USING hash (perceptual_hash)
    WHERE perceptual_hash IS NOT NULL;

CREATE TABLE collection_items (
    collection_id  uuid NOT NULL REFERENCES collections (id) ON DELETE CASCADE,
    blob_cid       text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    position       int NOT NULL DEFAULT 0,
    added_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (collection_id, blob_cid)
);

CREATE INDEX collection_items_position_idx
    ON collection_items (collection_id, position);

-- ----------------------------------------------------------------------------
-- Federated nodes (v2: + federation_cert_fingerprint, + reputation_score,
--                      + last_status_change_at)
--
-- Two cert fingerprints because Nova authenticates donors at two layers:
--   - Nebula cert: authorizes overlay membership
--   - Federation cert: authorizes HTTP API calls to /fed/v1
-- See FEDERATION_PROTOCOL.md for the mTLS-inside-Nebula design.
--
-- reputation_score is updated by Phase 2 possession audits; nodes with
-- high scores carry more healing work in the asymmetric source selection.
-- ----------------------------------------------------------------------------

CREATE TABLE nodes (
    id                              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    nebula_cert_fingerprint         text UNIQUE NOT NULL,
    federation_cert_fingerprint     text UNIQUE NOT NULL,
    display_name                    text,
    geo_declared                    text,
    capacity_bytes                  bigint NOT NULL CHECK (capacity_bytes >= 0),
    bandwidth_budget_bytes_per_day  bigint NOT NULL CHECK (bandwidth_budget_bytes_per_day >= 0),
    policy_filters                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                          node_status NOT NULL DEFAULT 'active',
    reputation_score                real NOT NULL DEFAULT 1.0
        CHECK (reputation_score >= 0.0 AND reputation_score <= 1.0),
    joined_at                       timestamptz NOT NULL DEFAULT now(),
    last_seen_at                    timestamptz,
    last_status_change_at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX nodes_status_idx ON nodes (status);
CREATE INDEX nodes_last_seen_idx ON nodes (last_seen_at);
CREATE INDEX nodes_reputation_idx ON nodes (reputation_score) WHERE status IN ('active', 'suspect');

-- P2-M0 (D8/D9) — Phase 2 `nodes` deltas (COMMENTARY; live DDL in P2-M3):
--   ALTER TABLE nodes ADD COLUMN donor_principal_id   uuid;        -- operator-verified owner (Sybil + anti-affinity)
--   ALTER TABLE nodes ADD COLUMN failure_domain_id    text;        -- operator-verified placement domain
--   ALTER TABLE nodes ADD COLUMN provider             text;        -- hosting provider (failure-domain dimension)
--   ALTER TABLE nodes ADD COLUMN asn                  integer;     -- autonomous system (failure-domain dimension)
--   ALTER TABLE nodes ADD COLUMN operator_verified_at timestamptz; -- when the operator verified the above
--   ALTER TABLE nodes ADD COLUMN trust_state          text NOT NULL DEFAULT 'probationary'
--       CHECK (trust_state IN ('probationary','trusted','suspended'));  -- D9: orthogonal to status + reputation
--   ALTER TABLE nodes ADD COLUMN placement_weight     real NOT NULL DEFAULT 0.0;  -- capped while probationary
-- Placement anti-affinity uses operator-verified failure_domain_id /
-- donor_principal_id, NOT self-declared geo_declared (which stays informational).
-- Steady-state placement weight is decoupled from bandwidth (HEALING D8);
-- bandwidth governs repair-source selection only.

-- ----------------------------------------------------------------------------
-- Pin assignments
--
-- The orchestrator's healing loop reads from this table to derive
-- under-replicated CIDs. See HEALING_PROTOCOL.md.
-- ----------------------------------------------------------------------------

CREATE TABLE pin_assignments (
    cid          text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    node_id      uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    state        pin_state NOT NULL DEFAULT 'pending',
    assigned_at  timestamptz NOT NULL DEFAULT now(),
    acked_at     timestamptz,

    PRIMARY KEY (cid, node_id)
);

CREATE INDEX pin_assignments_node_state_idx ON pin_assignments (node_id, state);
CREATE INDEX pin_assignments_state_idx ON pin_assignments (state);
CREATE INDEX pin_assignments_cid_state_idx ON pin_assignments (cid, state);

-- P2-M0 (D6/D7) — Phase 2 assignment-versioning + durable change log
-- (COMMENTARY; live DDL in P2-M3):
--   ALTER TABLE pin_assignments ADD COLUMN assignment_id uuid NOT NULL DEFAULT gen_random_uuid();
--   ALTER TABLE pin_assignments ADD COLUMN generation    bigint NOT NULL DEFAULT 1;
--   -- (cid, node_id) stays the natural current-assignment key; assignment_id is the
--   -- immutable handle carried in the change log + repair tokens + ack/fail. State
--   -- transitions are conditional, so a delayed ack/fail for a superseded
--   -- assignment is a no-op (D6):
--   --   UPDATE pin_assignments SET state='acked', acked_at=now()
--   --    WHERE assignment_id = $1 AND generation = $2 AND state = 'pending';
--
--   CREATE TABLE pin_changes (                       -- D7: durable change log backing since_seq
--       sequence      bigserial PRIMARY KEY,
--       node_id       uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
--       assignment_id uuid NOT NULL,
--       generation    bigint NOT NULL,
--       kind          text NOT NULL,                 -- 'assign' | 'unpin' (donor fails closed on unknown)
--       cid           text NOT NULL,
--       created_at    timestamptz NOT NULL DEFAULT now()
--   );
--   -- Retention window pruned; when a donor's since_seq predates retention the
--   -- coordinator returns snapshot_required (FEDERATION_PROTOCOL.md).
--
--   CREATE TABLE blob_replication_state (            -- durable projection: no per-tick full scans
--       cid                 text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
--       healthy_acked_count integer NOT NULL DEFAULT 0,
--       in_flight_count     integer NOT NULL DEFAULT 0,
--       target_count        integer NOT NULL,
--       safety_tier         smallint NOT NULL,        -- 1 = Tier-1 (acked-only), 2 = Tier-2
--       updated_at          timestamptz NOT NULL DEFAULT now()
--   );
--   -- Updated in the same transaction as assignment/liveness changes; a periodic
--   -- full reconciliation rebuilds it as a correctness audit (HEALING_PROTOCOL.md).
--
-- Forward-compat (NOT Phase 2): Phase 6 HA adds jobs.lease_id/lease_generation +
-- coordinator_leases(term) and origin_locations ADDITIVELY. Do NOT add them here.

-- ----------------------------------------------------------------------------
-- Pin audits (v2 — Phase 2 possession spot-checks)
--
-- See POSSESSION_AUDIT.md. Coordinator periodically challenges donors
-- to return random blocks from local storage only (no Bitswap fallback).
-- ----------------------------------------------------------------------------

CREATE TABLE pin_audits (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    blob_cid          text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    node_id           uuid NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    challenge_kind    text NOT NULL,                 -- 'block_hash', 'envelope_round_trip'
    nonce             text NOT NULL,
    deadline          timestamptz NOT NULL,
    result            audit_result,                  -- NULL until response received or timeout
    latency_ms        integer,
    bytes_verified    bigint,
    error             text,
    challenged_at     timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz
);

CREATE INDEX pin_audits_node_result_idx ON pin_audits (node_id, result, challenged_at DESC);
CREATE INDEX pin_audits_blob_idx ON pin_audits (blob_cid, challenged_at DESC);

-- P2-M0 (D10) — Phase 2 pin_audits deltas (COMMENTARY; live DDL shipped in migration 0015 / P2-M6):
--   ALTER TABLE pin_audits ADD COLUMN received_at timestamptz;  -- coordinator receive-time; AUTHORITATIVE for the deadline
--   -- completed_at (above) is donor-supplied and ADVISORY only; the pass/fail
--   -- deadline decision uses received_at, never completed_at (POSSESSION_AUDIT.md).
--   -- Sampling is weighted by stored bytes / pin count / node age / risk,
--   -- computed from nodes + pin_assignments counts (no new columns required).
--   -- P2-M6 (migration 0015) additionally added decided_at and transcript_hash; see P2-M6 section below.

-- ----------------------------------------------------------------------------
-- Integrity audits (v2 — Phase 1 local fixity)
--
-- See INTEGRITY_AUDIT.md. Coordinator-internal correctness checks
-- against its own local Kubo + DB state. No donor involvement; this is
-- about catching implementation bugs and silent corruption before
-- donors are involved.
-- ----------------------------------------------------------------------------

CREATE TABLE integrity_audits (
    id          bigserial PRIMARY KEY,
    cid         text NOT NULL,
    audit_kind  audit_kind NOT NULL,
    result      audit_result NOT NULL,
    error       text,
    audited_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX integrity_audits_cid_kind_idx ON integrity_audits (cid, audit_kind, audited_at DESC);
CREATE INDEX integrity_audits_failures_idx
    ON integrity_audits (audit_kind, audited_at DESC)
    WHERE result <> 'pass';

-- ----------------------------------------------------------------------------
-- Moderation (v2: + scheduled_tombstone_at, + legal_hold)
--
-- DMCA quarantine-first flow:
--   1. Notice qualifies → moderation_decisions row with action='quarantine',
--      scheduled_tombstone_at = now() + counter-notification window.
--   2. Blob state → 'quarantined'.
--   3. Background job processes overdue scheduled_tombstone_at rows:
--      verify legal_hold = false, then tombstone + crypto-shred.
--   4. If counter-notification arrives, scheduled_tombstone_at cleared
--      and operator decides to restore (state → 'active') or proceed.
--
-- Severe-content flow:
--   1. PDQ match against StopNCII → moderation_decisions with
--      action='quarantine', legal_hold=true, scheduled_tombstone_at=NULL.
--   2. Blob state → 'quarantined'; data_encryption_keys.legal_hold = true.
--   3. Operator generates external report (NCMEC CyberTipline).
--   4. After statutory preservation window + operator clearance,
--      manually clear legal_hold and set scheduled_tombstone_at.
-- ----------------------------------------------------------------------------

CREATE TABLE moderation_decisions (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cid                      text NOT NULL,
    rule                     moderation_rule NOT NULL,
    rule_ref                 text,
    action                   moderation_action NOT NULL,
    decided_by               uuid REFERENCES users (id),
    decided_at               timestamptz NOT NULL DEFAULT now(),
    scheduled_tombstone_at   timestamptz,        -- when set, background job tombstones at this time (subject to legal_hold)
    legal_hold               boolean NOT NULL DEFAULT false,
    notes                    text
);

CREATE INDEX moderation_decisions_cid_idx ON moderation_decisions (cid);
CREATE INDEX moderation_decisions_decided_at_idx ON moderation_decisions (decided_at DESC);
CREATE INDEX moderation_decisions_scheduled_tombstone_idx
    ON moderation_decisions (scheduled_tombstone_at)
    WHERE scheduled_tombstone_at IS NOT NULL;

CREATE TABLE dmca_cases (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    claimant_name     text NOT NULL,
    claimant_email    citext NOT NULL,
    sworn_statement   text NOT NULL,
    target_cid        text NOT NULL,
    received_at       timestamptz NOT NULL DEFAULT now(),
    actioned_at       timestamptz,
    status            dmca_status NOT NULL DEFAULT 'received'
);

CREATE INDEX dmca_cases_target_cid_idx ON dmca_cases (target_cid);
CREATE INDEX dmca_cases_status_idx ON dmca_cases (status);

CREATE TABLE takedown_repeat_infringers (
    user_id         uuid PRIMARY KEY REFERENCES users (id),
    strikes         int NOT NULL DEFAULT 1 CHECK (strikes > 0),
    last_strike_at  timestamptz NOT NULL DEFAULT now()
);

-- ----------------------------------------------------------------------------
-- Blocklist (M9 — operator/scanner-maintained CID blocklist)
--
-- Entries here are checked on every blob read (storage layer). A CID that
-- appears in this table is blocked regardless of blob.state; the read
-- handler returns 451 and logs an audit event. Distinct from
-- moderation_decisions: blocklist is a fast set-membership check; decisions
-- carry the full audit trail and scheduling state.
-- ----------------------------------------------------------------------------

CREATE TABLE blocklist (
    cid         text PRIMARY KEY,
    reason      text NOT NULL,
    rule        moderation_rule NOT NULL DEFAULT 'operator_manual',
    added_by    uuid REFERENCES users (id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX blocklist_created_at_idx ON blocklist (created_at DESC);

-- ----------------------------------------------------------------------------
-- Signed-URL revocations (v2 — structured)
--
-- The original prefix-string-against-canonical scheme was broken: the
-- canonical signed-URL string starts with the URL path (e.g.,
-- '/i/bafy.../p/thumb.webp\n...'), so prefixes like 'cid:bafy...' or
-- 'aud:https://example.com' could never match. This table stores
-- structured (kind, value) tuples; the verifier parses the canonical
-- string into its fields and checks each field against the revocations.
-- ----------------------------------------------------------------------------

CREATE TABLE signed_url_revocations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind        text NOT NULL CHECK (kind IN ('cid', 'aud', 'kid', 'path_prefix')),
    value       text NOT NULL,
    revoked_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (kind, value)
);

CREATE INDEX signed_url_revocations_kind_value_idx ON signed_url_revocations (kind, value);

-- ----------------------------------------------------------------------------
-- Audit log
-- ----------------------------------------------------------------------------

CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    actor_id     uuid REFERENCES users (id),
    action       text NOT NULL,
    target_type  text NOT NULL,
    target_id    text NOT NULL,
    payload      jsonb NOT NULL DEFAULT '{}'::jsonb,
    at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_target_idx ON audit_log (target_type, target_id);
CREATE INDEX audit_log_actor_idx ON audit_log (actor_id, at DESC);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);

-- ----------------------------------------------------------------------------
-- Updated-at trigger
-- ----------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ----------------------------------------------------------------------------
-- Bootstrap: initial master-key version
--
-- First-boot init must insert a master_key_versions row corresponding
-- to whatever NOVA_MASTER_KEY is loaded at startup. The version_label
-- is operator-chosen; 'v1' is conventional.
--
-- This file does not create the row itself (the value depends on the
-- runtime environment); cmd/migrate handles it on first deployment.
-- ----------------------------------------------------------------------------

-- ============================================================================
-- P2-M5 (migration 0014) — liveness & healing (as-built, D-M5-14)
-- ============================================================================

-- blob_replication_state: rebuildable donor-replica health projection (authority
-- remains pin_assignments ⨝ node liveness). Counts are acked-on-COUNTABLE-nodes
-- only (status IN active/suspect AND assignment_sync_state='current'); pending,
-- coordinator cache, and origin copies NEVER count toward R.
CREATE TABLE blob_replication_state (
    cid                    text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
    healthy_acked_count    integer NOT NULL DEFAULT 0,
    sourceable_acked_count integer NOT NULL DEFAULT 0,
    in_flight_count        integer NOT NULL DEFAULT 0,   -- pending reservations; never lift Tier-1
    target_count           integer NOT NULL,
    safety_tier            text NOT NULL CHECK (safety_tier IN ('donor_lost','tier1','tier2','healthy')),
    local_recoverable      boolean NOT NULL DEFAULT false,  -- coordinator holds a usable local copy
    durability_class       text NOT NULL CHECK (durability_class IN ('important','normal','cache')),
    dirty                  boolean NOT NULL DEFAULT false,  -- recompute-before-schedule (D-M5-2d)
    updated_at             timestamptz NOT NULL DEFAULT now()
);

-- Durable, bounded reconcile queue: a bulk node-status transition enqueues affected
-- CIDs (set-based) in its mutation tx; an idempotent drain recomputes them in batches.
CREATE TABLE blob_replication_reconcile_queue (
    cid         text PRIMARY KEY REFERENCES blobs(cid) ON DELETE CASCADE,
    reason      text NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

-- Durable, restart-stable webhook once-per-window suppression, scoped so distinct
-- subjects are not collapsed (D-M5-9a).
CREATE TABLE webhook_suppression (
    event_type    text NOT NULL,
    destination   text NOT NULL,
    scope_key     text NOT NULL,
    last_fired_at timestamptz NOT NULL,
    PRIMARY KEY (event_type, destination, scope_key)
);

-- nodes: D8/D9 placement + liveness/telemetry columns.
ALTER TABLE nodes
    ADD COLUMN failure_domain_id    text,        -- operator-verified only (else collapses to "unknown")
    ADD COLUMN donor_principal_id   text,
    ADD COLUMN provider             text,
    ADD COLUMN asn                  text,
    ADD COLUMN region               text,
    ADD COLUMN operator_verified_at timestamptz, -- set by `novactl node set-domain`
    ADD COLUMN placement_weight     real NOT NULL DEFAULT 1.0 CHECK (placement_weight BETWEEN 0.0 AND 1.0),
    ADD COLUMN assignment_sync_state text NOT NULL DEFAULT 'current'
        CHECK (assignment_sync_state IN ('current','snapshot_required','reconciling')),
    ADD COLUMN revoked_signaled_at   timestamptz,  -- node_revoked emit-once gate
    ADD COLUMN last_egress_remaining_bytes bigint,  -- best-effort step_capacity telemetry (hint only)
    ADD COLUMN last_egress_capacity_bytes  bigint,
    ADD COLUMN last_egress_refill_bps       bigint;

-- pin_assignments: durable repair-source binding that survives snapshot recovery.
ALTER TABLE pin_assignments
    ADD COLUMN source_node_id         uuid REFERENCES nodes(id),  -- NULL ⇒ coordinator-as-source
    ADD COLUMN source_attempts        integer NOT NULL DEFAULT 0,
    ADD COLUMN source_next_attempt_at timestamptz;                -- late-bind retry backoff

-- pin_changes: incremental copy of the repair source for change-log delivery + audit.
ALTER TABLE pin_changes ADD COLUMN source_node_id uuid REFERENCES nodes(id);

-- ============================================================================
-- P2-M6 (migration 0015) — possession audits & reputation (as-built, D-M6-2)
-- ============================================================================

-- pin_audits: received_at / decided_at / transcript_hash (D-M6-2a, D-M6-3a).
--
--   received_at    — coordinator response receive-time; NULL on timeout; AUTHORITATIVE
--                    deadline basis (D10). Never used for failure history (cannot anchor
--                    what was never received).
--   decided_at     — always set (pass / fail / skip / timeout); the indexing and
--                    operator-query column. Use this for audit history; never received_at.
--   transcript_hash — domain-separated, length-prefixed SHA-256 over challenge + block:
--                    sha256("NOVA-POSSESSION-AUDIT-v1" || 0x00
--                           || lp(challenge_id) || lp(blob_cid) || lp(assignment_id)
--                           || uint64be(generation) || lp(block_cid)
--                           || uint64be(block_index) || uint64be(block_size)
--                           || lp(nonce) || lp(block_bytes))
--                    where lp(x) = uint32be(len(x)) || x  (D-M6-3a).
ALTER TABLE pin_audits
    ADD COLUMN received_at      timestamptz,   -- NULL on timeout (D10)
    ADD COLUMN decided_at       timestamptz,   -- always set; use for indexing / queries (D-M6-2a)
    ADD COLUMN transcript_hash  bytea;         -- domain-separated audit transcript digest (D-M6-3a)

-- nodes: trust-epoch and review marker for graduation evidence (D-M6-2b, D-M6-8).
--
--   trust_epoch_started_at   — evidence-counting anchor; graduation counts only
--                              audits/transfers with timestamp ≥ this value.
--                              Backfilled to joined_at for existing donors at M6
--                              deploy (tenure is not restarted retroactively).
--                              Reset to now() on hash-mismatch (trust clock restarts).
--   trust_review_required_at — set by a hash-mismatch; blocks auto-graduation until
--                              an operator runs `novactl node trust clear-review`.
--   trust_review_reason      — human-readable reason surfaced to the operator.
ALTER TABLE nodes
    ADD COLUMN trust_epoch_started_at   timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN trust_review_required_at timestamptz,
    ADD COLUMN trust_review_reason      text;

-- Backfill: existing donors keep their tenure; epoch anchors to join time.
-- UPDATE nodes SET trust_epoch_started_at = joined_at;  (executed in migration; shown for reference)

-- EXPLAIN-gated indexes (D-M6-2c):
CREATE INDEX pin_assignments_acked_at_idx
    ON pin_assignments (acked_at) WHERE state = 'acked';         -- new-ack fast lane (D-M6-5b)
CREATE INDEX pin_audits_recent_pass_node_blob_idx
    ON pin_audits (node_id, blob_cid, received_at DESC) WHERE result = 'pass'; -- recent-pass tie-breaker (D-M6-9)
CREATE INDEX pin_audits_recent_fail_node_idx
    ON pin_audits (node_id, decided_at DESC) WHERE result = 'fail'; -- decided_at always set even on timeout (D-M6-2a)
