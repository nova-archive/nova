-- ============================================================================
-- Nova: Networked Object Versatile Archive
-- Data Model — Phase 0 (Postgres 16)
--
-- This file is the authoritative schema. Production migrations under
-- internal/db/migrations/ in Phase 1+ derive from this file; sqlc reads
-- the schema and generates the Go query layer.
--
-- Conventions:
--   - All timestamps are timestamptz, defaulted to now() where appropriate.
--   - All UUIDs are gen_random_uuid() unless explicitly user-supplied.
--   - All enum types live in the `nova` namespace via prefixed names.
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
    'quarantined',    -- moderation hold; reads return 451 Unavailable For Legal Reasons
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

CREATE TYPE node_status AS ENUM (
    'active',     -- heartbeating within tolerance
    'degraded',   -- missed heartbeats but not yet evicted
    'offline',    -- no heartbeat for max_offline_window
    'revoked'     -- operator marked compromised; cert revoked
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
    'operator_manual',  -- operator decision without scanner
    'user_report'       -- end-user reported the content
);

CREATE TYPE moderation_action AS ENUM (
    'quarantine',  -- block reads but retain bytes pending review
    'tombstone',   -- shred encryption key; broadcast unpin
    'allow'        -- explicitly clear (e.g., false-positive review)
);

CREATE TYPE dmca_status AS ENUM (
    'received',
    'investigating',
    'actioned',
    'rejected'
);

CREATE TYPE key_state AS ENUM (
    'active',
    'shredded'
);

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
-- Encryption keys
--
-- Per-blob symmetric keys (XChaCha20-Poly1305) are wrapped with the
-- operator master key (loaded from environment, never stored in this
-- table) and persisted as bytea. Crypto-shredding sets state='shredded'
-- and zeroes wrapped_key; the bytes still on volunteer disks become
-- unrecoverable ciphertext.
-- ----------------------------------------------------------------------------

CREATE TABLE keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    algorithm    text NOT NULL,           -- e.g. 'XChaCha20-Poly1305'
    wrapped_key  bytea NOT NULL,          -- zeroed on shred
    state        key_state NOT NULL DEFAULT 'active',
    created_at   timestamptz NOT NULL DEFAULT now(),
    shredded_at  timestamptz
);

CREATE INDEX keys_state_idx ON keys (state);

-- ----------------------------------------------------------------------------
-- Blobs (the storage core)
-- ----------------------------------------------------------------------------

CREATE TABLE blobs (
    cid                text PRIMARY KEY,
    encryption_key_id  uuid REFERENCES keys (id),
    owner_id           uuid REFERENCES users (id),
    mime_type          text NOT NULL,
    byte_size          bigint NOT NULL CHECK (byte_size >= 0),
    uploaded_at        timestamptz NOT NULL DEFAULT now(),
    state              blob_state NOT NULL DEFAULT 'active',
    source_ip          inet,
    product            blob_product NOT NULL DEFAULT 'raw'
);

CREATE INDEX blobs_owner_state_idx
    ON blobs (owner_id, state)
    WHERE state <> 'tombstoned';

CREATE INDEX blobs_uploaded_at_idx
    ON blobs (uploaded_at);

CREATE INDEX blobs_product_state_idx
    ON blobs (product, state);

-- ----------------------------------------------------------------------------
-- Image-specific metadata (nova-image product layer)
--
-- One row per CID that is an image. Future product layers add their own
-- side tables joined on cid; the storage core remains content-agnostic.
-- ----------------------------------------------------------------------------

CREATE TABLE image_metadata (
    cid              text PRIMARY KEY REFERENCES blobs (cid) ON DELETE CASCADE,
    width            int NOT NULL CHECK (width > 0),
    height           int NOT NULL CHECK (height > 0),
    perceptual_hash  bytea NOT NULL,    -- 32 bytes, PDQ
    alt_text         text,
    caption          text
);

-- The BK-tree index used for near-neighbor lookups is built in-memory at
-- coordinator startup from this column. The DB index here supports
-- exact-match deduplication queries and admin tooling only.
CREATE INDEX image_metadata_perceptual_hash_idx
    ON image_metadata USING hash (perceptual_hash);

-- ----------------------------------------------------------------------------
-- Derivatives (transform outputs cached as their own CIDs)
--
-- Each (parent_cid, preset, format) maps deterministically to one
-- derivative_cid. Recomputing the same transform yields the same CID
-- (transform engine version is pinned per release; bumping it produces
-- new derivative CIDs and old URLs continue resolving via the previous
-- derivative's row).
-- ----------------------------------------------------------------------------

CREATE TABLE derivatives (
    derivative_cid  text PRIMARY KEY,
    parent_cid      text NOT NULL REFERENCES blobs (cid) ON DELETE CASCADE,
    preset          text NOT NULL,
    format          text NOT NULL,
    byte_size       bigint NOT NULL CHECK (byte_size >= 0),
    produced_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (parent_cid, preset, format)
);

CREATE INDEX derivatives_parent_cid_idx ON derivatives (parent_cid);

-- ----------------------------------------------------------------------------
-- Collections
-- ----------------------------------------------------------------------------

CREATE TABLE collections (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id    uuid NOT NULL REFERENCES users (id),
    name        text NOT NULL,
    slug        text NOT NULL,
    visibility  collection_visibility NOT NULL DEFAULT 'private',
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (owner_id, slug)
);

CREATE INDEX collections_visibility_idx
    ON collections (visibility)
    WHERE visibility <> 'private';

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
-- Federated nodes (donor pinning nodes)
--
-- The Nebula certificate fingerprint is the durable identity. Node IDs
-- are operator-issued at registration and remain stable across reboots.
-- ----------------------------------------------------------------------------

CREATE TABLE nodes (
    id                              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    nebula_cert_fingerprint         text UNIQUE NOT NULL,
    display_name                    text,
    geo_declared                    text,    -- self-declared, unverified
    capacity_bytes                  bigint NOT NULL CHECK (capacity_bytes >= 0),
    bandwidth_budget_bytes_per_day  bigint NOT NULL CHECK (bandwidth_budget_bytes_per_day >= 0),
    policy_filters                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    status                          node_status NOT NULL DEFAULT 'active',
    joined_at                       timestamptz NOT NULL DEFAULT now(),
    last_seen_at                    timestamptz
);

CREATE INDEX nodes_status_idx ON nodes (status);
CREATE INDEX nodes_last_seen_idx ON nodes (last_seen_at);

-- ----------------------------------------------------------------------------
-- Pin assignments
--
-- The orchestrator's bandwidth-aware healing loop reads from this table
-- to derive Tier 1 (CIDs at < 2 acked pins) and Tier 2 (CIDs at < R acked
-- pins). See HEALING_PROTOCOL.md.
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
-- Moderation
-- ----------------------------------------------------------------------------

CREATE TABLE moderation_decisions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cid          text NOT NULL,                       -- not FK; CID may be tombstoned
    rule         moderation_rule NOT NULL,
    rule_ref     text,                                -- e.g. blocklist hash, DMCA case id
    action       moderation_action NOT NULL,
    decided_by   uuid REFERENCES users (id),
    decided_at   timestamptz NOT NULL DEFAULT now(),
    notes        text
);

CREATE INDEX moderation_decisions_cid_idx ON moderation_decisions (cid);
CREATE INDEX moderation_decisions_decided_at_idx ON moderation_decisions (decided_at DESC);

CREATE TABLE dmca_cases (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    claimant_name     text NOT NULL,
    claimant_email    citext NOT NULL,
    sworn_statement   text NOT NULL,
    target_cid        text NOT NULL,                  -- not FK; can outlive tombstone
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
-- Signed-URL revocations
--
-- Any canonical signed-URL string that starts with a stored prefix fails
-- verification. See SIGNED_URL_FORMAT.md, "Revocation".
-- ----------------------------------------------------------------------------

CREATE TABLE signed_url_revocations (
    prefix      text PRIMARY KEY,
    revoked_at  timestamptz NOT NULL DEFAULT now()
);

-- ----------------------------------------------------------------------------
-- Audit log
--
-- Every privileged action is logged. Never cascades. Append-only in
-- production (revoke DELETE/UPDATE for app role).
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
