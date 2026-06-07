import { describe, expect, it } from 'vitest'
import { challengeFromVerifier, randomState, randomVerifier } from './pkce'

describe('pkce', () => {
  it('computes the RFC 7636 Appendix B S256 challenge', async () => {
    const verifier = 'dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk'
    const challenge = await challengeFromVerifier(verifier)
    expect(challenge).toBe('E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM')
  })

  it('generates url-safe, unique verifiers and states', () => {
    expect(randomVerifier()).toMatch(/^[A-Za-z0-9_-]+$/)
    expect(randomState()).toMatch(/^[A-Za-z0-9_-]+$/)
    expect(randomVerifier()).not.toBe(randomVerifier())
    expect(randomVerifier().length).toBeGreaterThanOrEqual(43)
  })
})
