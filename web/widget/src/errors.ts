import type { WidgetError } from './api/types'

// mapError turns a finalize/commit HTTP failure into a stable WidgetError. The
// server's snake_case `code` (from internal/api/handlers/upload.go writePutError)
// is authoritative; the status table is the fallback when the body is empty.
export function mapError(status: number, body: { code?: string; message?: string } = {}): WidgetError {
  if (body.code) return { code: body.code, message: body.message }
  const byStatus: Record<number, string> = {
    400: 'bad_request',
    401: 'unauthenticated',
    403: 'forbidden',
    404: 'not_found',
    409: 'upload_incomplete',
    413: 'payload_too_large',
    415: 'unsupported_media_type',
    422: 'moderation_rejected',
    451: 'blocklisted',
    503: 'server_busy',
  }
  return { code: byStatus[status] ?? 'upload_failed', message: body.message }
}
