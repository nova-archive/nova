import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { AuthProvider, useAuth } from './AuthProvider'

// --- fake fetch -------------------------------------------------------------
// Minimal Response stand-in (Node 16 / jsdom have no global fetch/Response). Our
// code only touches .ok / .status / .text() / .json().
function res(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: 'x',
    text: async () => (body === undefined ? '' : JSON.stringify(body)),
    json: async () => body,
  }
}

type Handler = (init?: RequestInit) => ReturnType<typeof res>
let routes: Record<string, Handler>
const calls: Array<{ key: string; init?: RequestInit }> = []

function install() {
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString()
    const path = new URL(url, 'http://localhost').pathname
    const method = (init?.method ?? 'GET').toUpperCase()
    const key = `${method} ${path}`
    calls.push({ key, init })
    const h = routes[key]
    return (h ? h(init) : res({ error: { code: 'not_mocked', message: key } }, 404)) as unknown as Response
  })
  vi.stubGlobal('fetch', fn)
}

function jwt(ttlSec = 900): string {
  const payload = btoa(JSON.stringify({ exp: Math.floor(Date.now() / 1000) + ttlSec }))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '')
  return `h.${payload}.s`
}

function Consumer() {
  const a = useAuth()
  return (
    <div>
      <span data-testid="ready">{String(a.ready)}</span>
      <span data-testid="mode">{a.config?.mode ?? ''}</span>
      <span data-testid="user">{a.user?.email ?? ''}</span>
      <button onClick={() => void a.loginLocal('op@x', 'pw')}>login</button>
      <button
        onClick={() => void a.completeExternalLogin(new URLSearchParams('code=abc&state=st'))}
      >
        callback
      </button>
    </div>
  )
}

function renderAuth() {
  return render(
    <AuthProvider>
      <Consumer />
    </AuthProvider>,
  )
}

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  routes = {}
  calls.length = 0
  vi.unstubAllGlobals()
})

afterEach(() => cleanup())

describe('AuthProvider', () => {
  it('logs in via the local issuer and loads the user', async () => {
    routes = {
      'GET /api/v1/auth/config': () => res({ mode: 'local' }),
      'POST /api/v1/auth/login': () => res({ access_token: jwt(), refresh_token: 'r1', token_type: 'bearer' }),
      'GET /api/v1/users/me': () => res({ id: 'u1', email: 'op@x', role: 'operator' }),
    }
    install()
    renderAuth()

    await waitFor(() => expect(screen.getByTestId('ready')).toHaveTextContent('true'))
    expect(screen.getByTestId('mode')).toHaveTextContent('local')
    expect(screen.getByTestId('user')).toHaveTextContent('')

    fireEvent.click(screen.getByText('login'))
    await waitFor(() => expect(screen.getByTestId('user')).toHaveTextContent('op@x'))

    // The /users/me call carried the bearer.
    const meCall = calls.find((c) => c.key === 'GET /api/v1/users/me')
    expect((meCall?.init?.headers as Record<string, string>).authorization).toMatch(/^Bearer /)
  })

  it('detects external-OIDC mode and exchanges the PKCE code', async () => {
    sessionStorage.setItem(
      'nova.admin.pkce',
      JSON.stringify({ verifier: 'verifier-xyz', state: 'st', token_endpoint: 'https://idp.example/token' }),
    )
    routes = {
      'GET /api/v1/auth/config': () =>
        res({ mode: 'external', issuer_url: 'https://idp.example', client_id: 'nova' }),
      'POST /token': () => res({ access_token: jwt(), refresh_token: 'r1', token_type: 'bearer' }),
      'GET /api/v1/users/me': () => res({ id: 'u9', email: 'sso@idp', role: 'operator' }),
    }
    install()
    renderAuth()

    await waitFor(() => expect(screen.getByTestId('mode')).toHaveTextContent('external'))

    fireEvent.click(screen.getByText('callback'))
    await waitFor(() => expect(screen.getByTestId('user')).toHaveTextContent('sso@idp'))

    // The token exchange carried the code_verifier (PKCE).
    const tokenCall = calls.find((c) => c.key === 'POST /token')
    expect(String(tokenCall?.init?.body)).toContain('code_verifier=verifier-xyz')
    expect(String(tokenCall?.init?.body)).toContain('grant_type=authorization_code')
  })

  it('refreshes the access token on a 401 and retries', async () => {
    localStorage.setItem(
      'nova.admin.tokens',
      JSON.stringify({ access_token: jwt(), refresh_token: 'r0', expires_at: Date.now() + 600_000 }),
    )
    let meHits = 0
    routes = {
      'GET /api/v1/auth/config': () => res({ mode: 'local' }),
      'POST /api/v1/auth/refresh': () => res({ access_token: jwt(), refresh_token: 'r1', token_type: 'bearer' }),
      'GET /api/v1/users/me': () => {
        meHits += 1
        return meHits === 1 ? res({ error: { code: 'unauthenticated', message: 'expired' } }, 401) : res({ id: 'u1', email: 'op@x', role: 'operator' })
      },
    }
    install()
    renderAuth()

    await waitFor(() => expect(screen.getByTestId('user')).toHaveTextContent('op@x'))
    expect(calls.some((c) => c.key === 'POST /api/v1/auth/refresh')).toBe(true)
    expect(meHits).toBe(2)
  })
})
