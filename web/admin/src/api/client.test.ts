import { describe, expect, it, vi } from 'vitest'
import { createApi } from './client'

describe('createApi config methods', () => {
  it('getConfig GETs the admin config endpoint', async () => {
    const fetcher = vi.fn(
      async () =>
        new Response(JSON.stringify({ version: 3, config: {}, privacy_warnings: [], fields: {} }), {
          status: 200,
        }),
    )
    const api = createApi(fetcher as unknown as typeof fetch)
    const r = await api.getConfig()
    expect(r.version).toBe(3)
    expect(fetcher).toHaveBeenCalledWith('/api/v1/admin/config')
  })

  it('patchConfig sends an If-Match header when a version is given', async () => {
    const fetcher = vi.fn(
      async (_input: RequestInfo | URL, _init?: RequestInit) =>
        new Response(JSON.stringify({ version: 4, config: {}, privacy_warnings: [], fields: {} }), {
          status: 200,
        }),
    )
    const api = createApi(fetcher as unknown as typeof fetch)
    await api.patchConfig({ tos_url: 'https://x' }, 3)
    const init = fetcher.mock.calls[0][1]!
    expect(init.method).toBe('PATCH')
    expect((init.headers as Record<string, string>)['If-Match']).toBe('3')
    expect(JSON.parse(init.body as string)).toEqual({ tos_url: 'https://x' })
  })
})
