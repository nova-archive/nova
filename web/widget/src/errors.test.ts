import { describe, it, expect } from 'vitest'
import { mapError } from './errors'

describe('mapError', () => {
  it('prefers the server error code when present', () => {
    expect(mapError(400, { code: 'mime_rejected', message: 'bad' })).toEqual({ code: 'mime_rejected', message: 'bad' })
  })

  it('maps known statuses when no code is present', () => {
    expect(mapError(413, {}).code).toBe('payload_too_large')
    expect(mapError(401, {}).code).toBe('unauthenticated')
    expect(mapError(409, {}).code).toBe('upload_incomplete')
    expect(mapError(422, {}).code).toBe('moderation_rejected')
    expect(mapError(451, {}).code).toBe('blocklisted')
    expect(mapError(503, {}).code).toBe('server_busy')
  })

  it('falls back to upload_failed for an unmapped status', () => {
    expect(mapError(418, {}).code).toBe('upload_failed')
  })
})
