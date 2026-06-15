import type { FieldEffect, FieldMeta, OperatorConfig } from '../api/types'

// CURATED_FIELDS records the dotted paths the screen edits and the effect we
// expect for each (static fallback). The rendered badge prefers the live
// fields[path].effect from the API; a test asserts the two agree so they cannot
// silently drift from the M0.4 classification.
export const CURATED_FIELDS: Array<{ path: string; effect: FieldEffect }> = [
  { path: 'coordinator.record_source_ip', effect: 'restart' },
  { path: 'source_ip_retention_days', effect: 'restart' },
  { path: 'coordinator.public_ipfs_dht', effect: 'restart' },
  { path: 'auth.paranoid', effect: 'restart' },
  { path: 'uploads.cors.enabled', effect: 'live' },
  { path: 'uploads.cors.allowed_origins', effect: 'live' },
  { path: 'uploads.max_upload_size_bytes', effect: 'live' },
  { path: 'uploads.max_concurrent_assembly', effect: 'live' },
  { path: 'uploads.limits.max_concurrent_global', effect: 'live' },
  { path: 'uploads.limits.max_concurrent_per_session', effect: 'live' },
  { path: 'uploads.limits.max_files_per_session', effect: 'live' },
  { path: 'uploads.public_uploads', effect: 'restart' },
  { path: 'tos_url', effect: 'restart' },
]

export function effectFor(path: string, fields: Record<string, FieldMeta>): FieldEffect {
  return fields[path]?.effect ?? CURATED_FIELDS.find((f) => f.path === path)?.effect ?? 'restart'
}

// flattenConfig walks a nested config map to sorted dotted leaf paths + values.
export function flattenConfig(config: OperatorConfig): Array<{ path: string; value: unknown }> {
  const out: Array<{ path: string; value: unknown }> = []
  const walk = (prefix: string, v: unknown) => {
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      for (const [k, vv] of Object.entries(v as Record<string, unknown>)) {
        walk(prefix ? `${prefix}.${k}` : k, vv)
      }
      return
    }
    out.push({ path: prefix, value: v })
  }
  walk('', config)
  return out.sort((a, b) => a.path.localeCompare(b.path))
}
