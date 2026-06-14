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

  it('maps 429 → rate_limited when body has no code', () => {
    expect(mapError(429, {}).code).toBe('rate_limited')
  })

  it('body.code wins over 429 status fallback', () => {
    expect(mapError(429, { code: 'too_many_files', message: 'x' })).toEqual({ code: 'too_many_files', message: 'x' })
    expect(mapError(429, { code: 'too_many_concurrent', message: 'y' })).toEqual({ code: 'too_many_concurrent', message: 'y' })
  })

  it('503 remains server_busy (distinct from 429)', () => {
    expect(mapError(503, {}).code).toBe('server_busy')
  })
})
