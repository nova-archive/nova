import { beforeEach, describe, expect, it } from 'vitest'
import { clearTokens, jwtExp, loadTokens, saveTokens } from './storage'

function jwt(exp: number): string {
  const payload = btoa(JSON.stringify({ exp }))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
  return `h.${payload}.s`
}

describe('token storage', () => {
  beforeEach(() => localStorage.clear())

  it('round-trips tokens and derives expiry from the JWT exp', () => {
    const exp = Math.floor(Date.now() / 1000) + 900
    const stored = saveTokens({ access_token: jwt(exp), refresh_token: 'r1', token_type: 'bearer' })
    expect(stored.expires_at).toBe(exp * 1000)
    expect(loadTokens()?.refresh_token).toBe('r1')
    clearTokens()
    expect(loadTokens()).toBeNull()
  })

  it('falls back to expires_in when there is no JWT exp', () => {
    const stored = saveTokens({ access_token: 'opaque', refresh_token: 'r', token_type: 'bearer', expires_in: 60 })
    expect(stored.expires_at).toBeGreaterThan(Date.now())
    expect(stored.expires_at).toBeLessThanOrEqual(Date.now() + 60_000)
  })

  it('parses and rejects malformed JWTs', () => {
    expect(jwtExp(jwt(1234567890))).toBe(1234567890)
    expect(jwtExp('not-a-jwt')).toBeNull()
  })
})
