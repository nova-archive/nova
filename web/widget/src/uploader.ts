import type { NormalizedConfig } from './config'
import type { UploadResult, WidgetError } from './api/types'
import { resolveToken, authHeaders } from './auth'
import { mapError } from './errors'

export interface UploaderDeps {
  fetch: typeof fetch
}

// finalizeUpload performs the Nova-specific commit after tus transport success.
// uploadURL is the tus session URL (the tus Location, resolved absolute by the
// client). Only finalize yields a CID — tus "success" is transport only.
export async function finalizeUpload(
  uploadURL: string,
  cfg: NormalizedConfig,
  deps: UploaderDeps,
): Promise<UploadResult> {
  const token = await resolveToken(cfg)
  const resp = await deps.fetch(`${uploadURL}/finalize`, { method: 'POST', headers: authHeaders(token) })
  if (!resp.ok) {
    // A non-JSON error body (e.g. an nginx plain-text 413) is fine: mapError
    // still derives a stable code from the HTTP status.
    const body = await resp.json().catch(() => ({}))
    throw mapError(resp.status, body)
  }
  return (await resp.json()) as UploadResult
}

// A minimal emitter shape (satisfied by Uppy) so wireFinalize is testable without
// constructing a real Uppy/DragDrop instance.
export interface UploadSuccessEmitter {
  on(event: 'upload-success', cb: (file: unknown, resp: { uploadURL: string }) => void): void
}

// wireFinalize bridges transport-success → Nova finalize → onComplete/onError.
export function wireFinalize(emitter: UploadSuccessEmitter, cfg: NormalizedConfig, deps: UploaderDeps): void {
  emitter.on('upload-success', async (_file, resp) => {
    try {
      cfg.onComplete(await finalizeUpload(resp.uploadURL, cfg, deps))
    } catch (e) {
      // finalizeUpload throws a mapped WidgetError (a plain object) for a non-2xx
      // finalize; a network failure (fetch rejects) throws an Error instead —
      // normalize that so onError's contract (a defined e.code) always holds.
      cfg.onError(e instanceof Error ? { code: 'upload_failed', message: e.message } : (e as WidgetError))
    }
  })
}

export interface TusRequest {
  setHeader(name: string, value: string): void
}

// buildTusOptions returns the @uppy/tus options: endpoint, chunkSize, the metadata
// fields Nova reads, and per-request auth via the tus onBeforeRequest hook.
export function buildTusOptions(cfg: NormalizedConfig) {
  return {
    endpoint: cfg.endpoint,
    chunkSize: cfg.chunkSize,
    limit: cfg.concurrency,
    allowedMetaFields: ['filename', 'mime_type', 'product', 'collection_id'],
    async onBeforeRequest(req: TusRequest) {
      const token = await resolveToken(cfg)
      if (token) req.setHeader('Authorization', `Bearer ${token}`)
    },
  }
}

// fileMeta builds the tus Upload-Metadata for a file (collection_id only when set).
export function fileMeta(cfg: NormalizedConfig, file: { name: string; type: string }): Record<string, string> {
  return {
    filename: file.name,
    mime_type: file.type,
    product: cfg.product,
    ...(cfg.collectionId ? { collection_id: cfg.collectionId } : {}),
  }
}
