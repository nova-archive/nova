// API DTOs for the M11 surface the SPA consumes. Hand-written (focused subset of
// docs/specs/openapi.yaml) so the build is self-contained; kept in sync with the
// spec by review.

export type Role = 'viewer' | 'uploader' | 'moderator' | 'operator'

export interface AuthConfig {
  mode: 'local' | 'external'
  issuer_url?: string
  client_id?: string
  scopes?: string[]
}

export interface TokenResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in?: number
  kid?: string
}

export interface User {
  id: string
  email: string
  role: Role
  created_at?: string
  updated_at?: string
}

export interface Pagination {
  page: number
  per_page: number
  total: number
}

export interface Page<T> {
  data: T[]
  pagination: Pagination
}

export type BlobState = 'active' | 'soft_deleted' | 'quarantined' | 'tombstoned'
export type Product = 'image' | 'video' | 'audio' | 'archive' | 'document' | 'raw'

export interface AdminBlob {
  cid: string
  owner_id: string | null
  parent_cid: string | null
  mime_type: string
  byte_size: number
  state: BlobState
  product: Product
  uploaded_at: string
}

export interface BlobMeta extends AdminBlob {
  derivative_preset: string | null
  derivative_format: string | null
  soft_deleted_at: string | null
}

export type JobState = 'pending' | 'leased' | 'completed' | 'failed' | 'dead'

export interface Job {
  id: string
  kind: string
  state: JobState
  attempts: number
  max_attempts: number
  last_error: string | null
  not_before: string
  lease_until: string | null
  created_at: string
  updated_at: string
}

export interface ModerationDecision {
  id: string
  cid: string
  rule: string
  rule_ref: string | null
  action: string
  decided_by: string | null
  decided_at: string
  scheduled_tombstone_at: string | null
  legal_hold: boolean
  notes: string | null
}

export interface DmcaCase {
  id: string
  claimant_name: string
  claimant_email: string
  target_cid: string | null
  received_at: string
  actioned_at: string | null
  status: string
  sworn_statement?: string
}

export interface BlocklistEntry {
  cid: string
  reason: string
  rule: string
  added_by: string | null
  created_at: string
}

export type AuditResult = 'pass' | 'fail' | 'skip'

export interface IntegrityAudit {
  id: number
  cid: string
  audit_kind: string
  result: AuditResult
  error: string | null
  audited_at: string
}

export interface AuditLogEntry {
  id: string
  action: string
  target_type: string
  target_id: string
  actor_id: string | null
  created_at: string
  payload?: Record<string, unknown>
}

export interface VersionSummary {
  label: string
  state: string
  dek_count: number
  signing_count: number
  retired_at: string | null
}

export interface RotationStatus {
  active: string
  in_progress: {
    from: string
    remaining_deks: number
    remaining_signing_keys: number
    stalled: boolean
    stall_reason: string | null
  } | null
  versions: VersionSummary[]
}

// --- M0.6 runtime config (operator.yaml surface via the M0.4 admin API) -------

export type FieldEffect = 'live' | 'restart' | 'env-only-inert'

export interface FieldMeta {
  effect: FieldEffect
  source: 'yaml' | 'env'
  shadowed_by_env?: boolean
}

// OperatorConfig types only the curated leaves the Settings screen edits; the long
// tail stays index-typed for the read-only effective-config viewer.
export interface OperatorConfig {
  coordinator?: { record_source_ip?: boolean | null; public_ipfs_dht?: boolean }
  auth?: { paranoid?: boolean }
  source_ip_retention_days?: number
  tos_url?: string
  webhooks?: unknown[]
  uploads?: {
    max_upload_size_bytes?: number
    max_concurrent_assembly?: number
    public_uploads?: boolean
    cors?: { enabled?: boolean; allowed_origins?: string[] }
    limits?: {
      max_concurrent_global?: number
      max_concurrent_per_session?: number
      max_files_per_session?: number
    }
  }
  [section: string]: unknown
}

export interface ConfigResponse {
  version: number
  config: OperatorConfig
  privacy_warnings: string[]
  fields: Record<string, FieldMeta>
  restart_required?: string[]
}
