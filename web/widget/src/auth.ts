import type { NormalizedConfig } from './config'

// resolveToken awaits the caller's getToken PER REQUEST so a long resumable upload
// survives the 15-minute access-token expiry; an empty/null result means no
// Authorization header (the public-uploads floor).
export async function resolveToken(cfg: Pick<NormalizedConfig, 'getToken'>): Promise<string | null> {
  const tok = await cfg.getToken()
  return tok && tok.length > 0 ? tok : null
}

export function authHeaders(token: string | null): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {}
}
