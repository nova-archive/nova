import { describe, it, expect, vi } from 'vitest'
import { resolveToken, authHeaders } from './auth'

describe('resolveToken', () => {
  it('returns the token from getToken', async () => {
    expect(await resolveToken({ getToken: () => 'jwt' })).toBe('jwt')
  })

  it('awaits an async getToken', async () => {
    expect(await resolveToken({ getToken: async () => 'async-jwt' })).toBe('async-jwt')
  })

  it('treats empty/null as no token', async () => {
    expect(await resolveToken({ getToken: () => '' })).toBeNull()
    expect(await resolveToken({ getToken: () => null })).toBeNull()
  })

  it('is called per invocation — honors a rotated token', async () => {
    const getToken = vi.fn().mockReturnValueOnce('t1').mockReturnValueOnce('t2')
    expect(await resolveToken({ getToken })).toBe('t1')
    expect(await resolveToken({ getToken })).toBe('t2')
    expect(getToken).toHaveBeenCalledTimes(2)
  })
})

describe('authHeaders', () => {
  it('builds a Bearer header for a token', () => {
    expect(authHeaders('jwt')).toEqual({ Authorization: 'Bearer jwt' })
  })
  it('returns no header for a null token', () => {
    expect(authHeaders(null)).toEqual({})
  })
})
