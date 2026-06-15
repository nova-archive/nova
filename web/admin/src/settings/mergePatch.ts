import type { OperatorConfig } from '../api/types'

// SettingsDraft is the flat, typed local edit state for the curated controls.
// Protective privacy flags: true = hardened. retentionDays holds the raw value so
// an operator's hand-edited non-default (e.g. 7) is never clobbered.
export interface SettingsDraft {
  hardenNoIPRecording: boolean // record_source_ip = !flag
  retentionDays: number // source_ip_retention_days (raw; protective ⇔ ≤1)
  hardenPrivateDHT: boolean // public_ipfs_dht = !flag
  corsEnabled: boolean
  corsOrigins: string[]
  maxUploadSizeBytes: number
  maxConcurrentAssembly: number
  maxConcurrentGlobal: number
  maxConcurrentPerSession: number
  maxFilesPerSession: number
  publicUploads: boolean
  tosUrl: string
}

const DEFAULT_RETENTION = 30

export function draftFromConfig(c: OperatorConfig): SettingsDraft {
  const u = c.uploads ?? {}
  const cors = u.cors ?? {}
  const limits = u.limits ?? {}
  const retention = c.source_ip_retention_days ?? DEFAULT_RETENTION
  return {
    hardenNoIPRecording: c.coordinator?.record_source_ip === false,
    retentionDays: retention,
    hardenPrivateDHT: c.coordinator?.public_ipfs_dht === false,
    corsEnabled: cors.enabled === true,
    corsOrigins: [...(cors.allowed_origins ?? [])],
    maxUploadSizeBytes: u.max_upload_size_bytes ?? 0,
    maxConcurrentAssembly: u.max_concurrent_assembly ?? 0,
    maxConcurrentGlobal: limits.max_concurrent_global ?? 0,
    maxConcurrentPerSession: limits.max_concurrent_per_session ?? 0,
    maxFilesPerSession: limits.max_files_per_session ?? 0,
    publicUploads: u.public_uploads === true,
    tosUrl: c.tos_url ?? '',
  }
}

export function webhooksConfigured(c: OperatorConfig): boolean {
  return Array.isArray(c.webhooks) && c.webhooks.length > 0
}

// deriveParanoid: the screen writes auth.paranoid only as the AND of every
// protective child AND webhooks-empty, so a save can never satisfy the
// ApplyPrivacyPreset warn conditions (verified against internal/config/paranoid.go).
export function deriveParanoid(d: SettingsDraft, webhooks: boolean): boolean {
  return d.hardenNoIPRecording && d.retentionDays <= 1 && d.hardenPrivateDHT && !webhooks
}

type Patch = Record<string, unknown>
function setPath(patch: Patch, path: string[], val: unknown): void {
  let node = patch
  for (let i = 0; i < path.length - 1; i++) {
    node[path[i]] = (node[path[i]] as Patch) ?? {}
    node = node[path[i]] as Patch
  }
  node[path[path.length - 1]] = val
}
function arrayEq(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

// buildConfigPatch emits a minimal JSON Merge Patch: only leaves that changed
// from `initial`, nested under their sections, never `null`. When any privacy
// child changed it also emits the re-derived auth.paranoid.
export function buildConfigPatch(
  draft: SettingsDraft,
  initial: SettingsDraft,
  webhooks: boolean,
): Patch {
  const p: Patch = {}
  const privacyChanged =
    draft.hardenNoIPRecording !== initial.hardenNoIPRecording ||
    draft.retentionDays !== initial.retentionDays ||
    draft.hardenPrivateDHT !== initial.hardenPrivateDHT

  if (draft.hardenNoIPRecording !== initial.hardenNoIPRecording)
    setPath(p, ['coordinator', 'record_source_ip'], !draft.hardenNoIPRecording)
  if (draft.retentionDays !== initial.retentionDays)
    setPath(p, ['source_ip_retention_days'], draft.retentionDays)
  if (draft.hardenPrivateDHT !== initial.hardenPrivateDHT)
    setPath(p, ['coordinator', 'public_ipfs_dht'], !draft.hardenPrivateDHT)
  if (privacyChanged) setPath(p, ['auth', 'paranoid'], deriveParanoid(draft, webhooks))

  if (draft.corsEnabled !== initial.corsEnabled)
    setPath(p, ['uploads', 'cors', 'enabled'], draft.corsEnabled)
  if (!arrayEq(draft.corsOrigins, initial.corsOrigins))
    setPath(p, ['uploads', 'cors', 'allowed_origins'], draft.corsOrigins)

  if (draft.maxUploadSizeBytes !== initial.maxUploadSizeBytes)
    setPath(p, ['uploads', 'max_upload_size_bytes'], draft.maxUploadSizeBytes)
  if (draft.maxConcurrentAssembly !== initial.maxConcurrentAssembly)
    setPath(p, ['uploads', 'max_concurrent_assembly'], draft.maxConcurrentAssembly)
  if (draft.maxConcurrentGlobal !== initial.maxConcurrentGlobal)
    setPath(p, ['uploads', 'limits', 'max_concurrent_global'], draft.maxConcurrentGlobal)
  if (draft.maxConcurrentPerSession !== initial.maxConcurrentPerSession)
    setPath(p, ['uploads', 'limits', 'max_concurrent_per_session'], draft.maxConcurrentPerSession)
  if (draft.maxFilesPerSession !== initial.maxFilesPerSession)
    setPath(p, ['uploads', 'limits', 'max_files_per_session'], draft.maxFilesPerSession)

  if (draft.publicUploads !== initial.publicUploads)
    setPath(p, ['uploads', 'public_uploads'], draft.publicUploads)
  if (draft.tosUrl !== initial.tosUrl) setPath(p, ['tos_url'], draft.tosUrl)

  return p
}

// normalizeOrigin reduces operator input to a bare browser origin
// (scheme://host[:port]); returns null for non-http(s) or unparseable input.
// The CORS middleware exact-matches the Origin header, so a trailing slash or
// path would never match (internal/api/middleware/cors.go).
export function normalizeOrigin(input: string): string | null {
  const raw = input.trim()
  if (!raw) return null
  try {
    const u = new URL(raw)
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
    return u.origin
  } catch {
    return null
  }
}
