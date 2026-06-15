import type {
  AdminBlob,
  AuditLogEntry,
  BlobMeta,
  BlocklistEntry,
  ConfigResponse,
  DmcaCase,
  IntegrityAudit,
  Job,
  ModerationDecision,
  Page,
  RotationStatus,
  User,
} from './types'

// ApiError carries the coordinator's structured { error: { code, message } } so
// screens can branch on code (e.g. not_active → 409) and show message.
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

async function parse<T>(res: Response): Promise<T> {
  if (res.status === 204) return undefined as T
  const text = await res.text()
  const body = text ? JSON.parse(text) : undefined
  if (!res.ok) {
    const err = (body && (body.error ?? body)) || {}
    throw new ApiError(res.status, err.code ?? 'error', err.message ?? res.statusText)
  }
  return body as T
}

function qs(params?: Record<string, string | number | undefined>): string {
  if (!params) return ''
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ''
}

export interface ListQuery {
  page?: number
  per_page?: number
  [k: string]: string | number | undefined
}

// createApi builds the typed client over a fetcher (the AuthProvider injects the
// bearer + 401 handling). The content paths (/api/v1, /blob, /i) are same-origin.
export function createApi(fetcher: typeof fetch) {
  const get = <T>(path: string) => fetcher(path).then((r) => parse<T>(r))
  const send = <T>(method: string, path: string, body?: unknown) =>
    fetcher(path, {
      method,
      headers: body === undefined ? undefined : { 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    }).then((r) => parse<T>(r))

  const cidPath = (base: string, cid: string) => `${base}/${encodeURIComponent(cid)}`

  return {
    me: () => get<User>('/api/v1/users/me'),

    listBlobs: (q: ListQuery) => get<Page<AdminBlob>>('/api/v1/admin/blobs' + qs(q)),
    getBlob: (cid: string) => get<BlobMeta>(cidPath('/api/v1/blobs', cid)),
    softDeleteBlob: (cid: string) => send<void>('DELETE', cidPath('/api/v1/blobs', cid)),

    listJobs: (q: ListQuery) => get<Page<Job>>('/api/v1/admin/jobs' + qs(q)),

    moderationQueue: (q: ListQuery) =>
      get<Page<ModerationDecision>>('/api/v1/admin/moderation/queue' + qs(q)),
    quarantine: (b: Record<string, unknown>) =>
      send('POST', '/api/v1/admin/moderation/quarantine', b),
    takedown: (b: Record<string, unknown>) =>
      send('POST', '/api/v1/admin/moderation/takedown', b),
    restore: (b: Record<string, unknown>) =>
      send('POST', '/api/v1/admin/moderation/restore', b),
    counterNotice: (b: Record<string, unknown>) =>
      send('POST', '/api/v1/admin/moderation/counter-notice', b),

    dmcaCases: (q: ListQuery) => get<Page<DmcaCase>>('/api/v1/admin/dmca' + qs(q)),
    dmcaCase: (id: string) => get<DmcaCase>(cidPath('/api/v1/admin/dmca', id)),

    blocklist: (q: ListQuery) =>
      get<Page<BlocklistEntry>>('/api/v1/admin/moderation/blocklist' + qs(q)),
    addBlocklist: (b: { cid: string; reason: string }) =>
      send('POST', '/api/v1/admin/moderation/blocklist', b),
    removeBlocklist: (cid: string) =>
      send<void>('DELETE', cidPath('/api/v1/admin/moderation/blocklist', cid)),

    integrityAudits: (q: ListQuery) =>
      get<Page<IntegrityAudit>>('/api/v1/admin/audits/integrity' + qs(q)),
    auditLog: (q: ListQuery) => get<Page<AuditLogEntry>>('/api/v1/admin/audit-log' + qs(q)),

    rotationStatus: () => get<RotationStatus>('/api/v1/admin/keys/rotation-status'),
    rotateMaster: (b: { from_version: string; to_version: string }) =>
      send('POST', '/api/v1/admin/keys/rotate-master', b),
    rotateSigning: (b: { grace_seconds?: number }) =>
      send('POST', '/api/v1/admin/keys/rotate-signing', b),

    getConfig: () => get<ConfigResponse>('/api/v1/admin/config'),
    patchConfig: (patch: unknown, ifMatch?: number) =>
      fetcher('/api/v1/admin/config', {
        method: 'PATCH',
        headers: {
          'content-type': 'application/json',
          ...(ifMatch !== undefined ? { 'If-Match': String(ifMatch) } : {}),
        },
        body: JSON.stringify(patch),
      }).then((r) => parse<ConfigResponse>(r)),
  }
}

export type Api = ReturnType<typeof createApi>
