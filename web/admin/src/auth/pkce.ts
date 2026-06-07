// PKCE (RFC 7636) helpers for the external-OIDC authorization-code flow. The
// challenge is computed in the browser; the verifier never leaves it until the
// token exchange. Pure functions over Web Crypto so they unit-test cleanly.

function base64url(bytes: Uint8Array): string {
  let s = ''
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i])
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function randomBase64url(len: number): string {
  const a = new Uint8Array(len)
  crypto.getRandomValues(a)
  return base64url(a)
}

export function randomVerifier(): string {
  return randomBase64url(32)
}

export function randomState(): string {
  return randomBase64url(16)
}

export async function challengeFromVerifier(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier))
  return base64url(new Uint8Array(digest))
}
