import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'

import { ApiError, createApi, type Api } from '../api/client'
import type { AuthConfig, TokenResponse, User } from '../api/types'
import { challengeFromVerifier, randomState, randomVerifier } from './pkce'
import { clearTokens, loadTokens, saveTokens, type StoredTokens } from './storage'

interface AuthValue {
  ready: boolean
  config: AuthConfig | null
  user: User | null
  error: string | null
  api: Api
  loginLocal: (username: string, password: string) => Promise<void>
  beginExternalLogin: () => Promise<void>
  completeExternalLogin: (params: URLSearchParams) => Promise<void>
  logout: () => Promise<void>
}

const Ctx = createContext<AuthValue | null>(null)

export function useAuth(): AuthValue {
  const v = useContext(Ctx)
  if (!v) throw new Error('useAuth must be used within <AuthProvider>')
  return v
}

const REFRESH_SKEW_MS = 30_000
const PKCE_KEY = 'nova.admin.pkce'

export function AuthProvider({ children }: { children: ReactNode }) {
  const tokensRef = useRef<StoredTokens | null>(loadTokens())
  const configRef = useRef<AuthConfig | null>(null)
  const [config, setConfig] = useState<AuthConfig | null>(null)
  const [user, setUser] = useState<User | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [ready, setReady] = useState(false)

  const setTokens = useCallback((t: StoredTokens | null) => {
    tokensRef.current = t
  }, [])

  // Local-issuer refresh-token rotation. On failure the session is cleared so the
  // login screen reappears.
  const refreshLocal = useCallback(async (): Promise<StoredTokens | null> => {
    const cur = tokensRef.current
    if (!cur) return null
    const res = await fetch('/api/v1/auth/refresh', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ refresh_token: cur.refresh_token }),
    })
    if (!res.ok) {
      setTokens(null)
      clearTokens()
      return null
    }
    const next = saveTokens((await res.json()) as TokenResponse)
    setTokens(next)
    return next
  }, [setTokens])

  // authedFetch attaches the bearer, proactively refreshes a near-expired local
  // access token, and retries once on a 401 (refreshing in local mode).
  const authedFetch = useCallback<typeof fetch>(
    async (input, init) => {
      let tokens = tokensRef.current
      if (
        tokens &&
        tokens.expires_at - Date.now() < REFRESH_SKEW_MS &&
        configRef.current?.mode === 'local'
      ) {
        tokens = (await refreshLocal()) ?? tokens
      }
      const withAuth = (t: StoredTokens | null): RequestInit => ({
        ...init,
        headers: {
          ...((init?.headers as Record<string, string>) ?? {}),
          ...(t ? { authorization: `Bearer ${t.access_token}` } : {}),
        },
      })
      let res = await fetch(input, withAuth(tokens))
      if (res.status === 401 && tokens) {
        const refreshed = configRef.current?.mode === 'local' ? await refreshLocal() : null
        if (refreshed) {
          res = await fetch(input, withAuth(refreshed))
        } else {
          setTokens(null)
          clearTokens()
          setUser(null)
        }
      }
      return res
    },
    [refreshLocal, setTokens],
  )

  const api = useMemo(() => createApi(authedFetch), [authedFetch])

  const loadUser = useCallback(async () => {
    try {
      setUser(await api.me())
      setError(null)
    } catch {
      setUser(null)
    }
  }, [api])

  // Bootstrap: discover auth mode, then validate any stored session.
  useEffect(() => {
    let alive = true
    void (async () => {
      let cfg: AuthConfig = { mode: 'local' }
      try {
        const res = await fetch('/api/v1/auth/config')
        if (res.ok) cfg = (await res.json()) as AuthConfig
      } catch {
        // default to local
      }
      if (!alive) return
      configRef.current = cfg
      setConfig(cfg)
      if (tokensRef.current) await loadUser()
      if (alive) setReady(true)
    })()
    return () => {
      alive = false
    }
  }, [loadUser])

  const loginLocal = useCallback(
    async (username: string, password: string) => {
      setError(null)
      const res = await fetch('/api/v1/auth/login', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username, password }),
      })
      if (!res.ok) {
        const message = res.status === 401 ? 'Invalid credentials.' : `Login failed (${res.status}).`
        setError(message)
        throw new ApiError(res.status, 'login_failed', message)
      }
      setTokens(saveTokens((await res.json()) as TokenResponse))
      await loadUser()
    },
    [loadUser, setTokens],
  )

  const discover = useCallback(async (issuer: string) => {
    const res = await fetch(issuer.replace(/\/$/, '') + '/.well-known/openid-configuration')
    if (!res.ok) throw new Error('OIDC discovery failed')
    return (await res.json()) as { authorization_endpoint: string; token_endpoint: string }
  }, [])

  const beginExternalLogin = useCallback(async () => {
    const cfg = configRef.current
    if (!cfg || cfg.mode !== 'external' || !cfg.issuer_url) {
      throw new Error('external OIDC is not configured')
    }
    const meta = await discover(cfg.issuer_url)
    const verifier = randomVerifier()
    const state = randomState()
    sessionStorage.setItem(
      PKCE_KEY,
      JSON.stringify({ verifier, state, token_endpoint: meta.token_endpoint }),
    )
    const redirect_uri = window.location.origin + '/admin/callback'
    const challenge = await challengeFromVerifier(verifier)
    const url = new URL(meta.authorization_endpoint)
    url.searchParams.set('response_type', 'code')
    url.searchParams.set('client_id', cfg.client_id ?? 'nova')
    url.searchParams.set('redirect_uri', redirect_uri)
    url.searchParams.set('scope', (cfg.scopes ?? ['openid', 'profile', 'email']).join(' '))
    url.searchParams.set('state', state)
    url.searchParams.set('code_challenge', challenge)
    url.searchParams.set('code_challenge_method', 'S256')
    window.location.assign(url.toString())
  }, [discover])

  const completeExternalLogin = useCallback(
    async (params: URLSearchParams) => {
      const raw = sessionStorage.getItem(PKCE_KEY)
      if (!raw) throw new Error('missing PKCE state')
      const saved = JSON.parse(raw) as { verifier: string; state: string; token_endpoint: string }
      const code = params.get('code')
      const state = params.get('state')
      if (!code || !state || state !== saved.state) throw new Error('invalid OIDC callback')
      const redirect_uri = window.location.origin + '/admin/callback'
      const res = await fetch(saved.token_endpoint, {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'authorization_code',
          code,
          redirect_uri,
          client_id: configRef.current?.client_id ?? 'nova',
          code_verifier: saved.verifier,
        }),
      })
      if (!res.ok) throw new Error('token exchange failed')
      setTokens(saveTokens((await res.json()) as TokenResponse))
      sessionStorage.removeItem(PKCE_KEY)
      await loadUser()
    },
    [loadUser, setTokens],
  )

  const logout = useCallback(async () => {
    const cur = tokensRef.current
    if (cur && configRef.current?.mode === 'local') {
      await fetch('/api/v1/auth/logout', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ refresh_token: cur.refresh_token }),
      }).catch(() => {})
    }
    setTokens(null)
    clearTokens()
    setUser(null)
  }, [setTokens])

  const value: AuthValue = {
    ready,
    config,
    user,
    error,
    api,
    loginLocal,
    beginExternalLogin,
    completeExternalLogin,
    logout,
  }

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}
