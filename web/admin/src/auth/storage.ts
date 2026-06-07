import type { TokenResponse } from '../api/types'

const KEY = 'nova.admin.tokens'

export interface StoredTokens {
  access_token: string
  refresh_token: string
  // Absolute epoch ms the access token expires (from the JWT exp claim, falling
  // back to expires_in) — drives proactive silent refresh.
  expires_at: number
}

export function loadTokens(): StoredTokens | null {
  try {
    const raw = localStorage.getItem(KEY)
    return raw ? (JSON.parse(raw) as StoredTokens) : null
  } catch {
    return null
  }
}

export function saveTokens(t: TokenResponse): StoredTokens {
  const stored: StoredTokens = {
    access_token: t.access_token,
    refresh_token: t.refresh_token,
    expires_at: accessExpiry(t),
  }
  localStorage.setItem(KEY, JSON.stringify(stored))
  return stored
}

export function clearTokens(): void {
  localStorage.removeItem(KEY)
}

function accessExpiry(t: TokenResponse): number {
  const exp = jwtExp(t.access_token)
  if (exp) return exp * 1000
  if (t.expires_in) return Date.now() + t.expires_in * 1000
  return Date.now() + 10 * 60 * 1000
}

export function jwtExp(token: string): number | null {
  const parts = token.split('.')
  if (parts.length < 2) return null
  try {
    const payload = JSON.parse(b64urlDecode(parts[1])) as { exp?: number }
    return typeof payload.exp === 'number' ? payload.exp : null
  } catch {
    return null
  }
}

function b64urlDecode(s: string): string {
  const pad = s.length % 4 === 0 ? '' : '='.repeat(4 - (s.length % 4))
  return atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad)
}
