-- +goose Up
-- +goose StatementBegin
-- Forward-only migration 0001: initial schema per docs/specs/DATA_MODEL.sql
-- This migration MUST remain bit-identical to docs/specs/DATA_MODEL.sql.
-- Drift fails CI (see .github/workflows/ci.yml: schema-drift check).
-- ============================================================================
-- Nova: Networked Object Versatile Archive
-- Data Model — Phase 0 v2 (Postgres 16)
--
-- This file is the authoritative schema. Production migrations under
-- internal/db/migrations/ in Phase 1+ derive from this file; sqlc reads
-- the schema and generates the Go query layer.
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
--     possession audits and Phase 6+ formal POR/PDP)
--   - integrity_audits + pin_audits + node reputation added for
--     Phase 1 local fixity and Phase 2 donor spot-checks
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

-- Key state values (v2). 'rotating' is a transient state during master-key
-- rotation; rows in this state are being re-wrapped under the new master key.
CREATE TYPE key_state AS ENUM (
    'active',
    'rotating',
    'shredded',
    'retired'      -- signing keys past their grace window
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
-- history. Each per-blob and per-signing key rows reference the
-- master-key version they were wrapped under. Rotation creates a new
-- 'active' row, marks the previous 'retired', and re-wraps every
-- referenced wrapped_key under the new master key in a single
-- transaction.
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
-- rotation can find rows that need re-wrapping.
--
-- legal_hold = true prevents crypto-shredding (the wrapped_key is not
-- zeroed and state remains 'active' or 'rotating') regardless of
-- blob.state. This is required for severe-content preservation flows
-- where evidence must be retained under statutory obligation.
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
-- Signed-URL revocations (v2 — structured)
--
-- The original prefix-string-against-canonical scheme was broken: the
-- canonical signed-URL string starts with the URL path (e.g.,
-- '/i/bafy.../p/thumb.webp<LF>...'), so prefixes like 'cid:bafy...' or
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
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Down-migration not provided. Phase 1 deployments treat schema rollback
-- as a restore-from-backup operation; the forward migration is the only
-- path through 0001.
SELECT 1;
-- +goose StatementEnd
