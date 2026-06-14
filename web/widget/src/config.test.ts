import { describe, it, expect } from 'vitest'
import { normalizeOptions, parseElementConfig } from './config'

describe('normalizeOptions', () => {
  it('applies defaults', () => {
    const c = normalizeOptions()
    expect(c.endpoint).toBe('/api/v1/uploads')
    expect(c.product).toBe('raw')
    expect(c.collectionId).toBeUndefined()
    expect(c.chunkSize).toBe(5 * 1024 * 1024)
    expect(c.concurrency).toBe(4)
    expect(c.maxFilesPerSession).toBe(100)
  })

  it('overrides endpoint/product/collection/chunk', () => {
    const c = normalizeOptions({ endpoint: '/x', product: 'image', collectionId: 'abc', chunkSize: 100 })
    expect(c.endpoint).toBe('/x')
    expect(c.product).toBe('image')
    expect(c.collectionId).toBe('abc')
    expect(c.chunkSize).toBe(100)
  })

  it('overrides concurrency and maxFilesPerSession when provided', () => {
    const c = normalizeOptions({ concurrency: 2, maxFilesPerSession: 50 })
    expect(c.concurrency).toBe(2)
    expect(c.maxFilesPerSession).toBe(50)
  })

  it('token: t becomes getToken returning t', async () => {
    const c = normalizeOptions({ token: 'jwt' })
    expect(await c.getToken()).toBe('jwt')
  })

  it('getToken wins when both token and getToken are set', async () => {
    const c = normalizeOptions({ token: 'static', getToken: () => 'dynamic' })
    expect(await c.getToken()).toBe('dynamic')
  })

  it('defaults getToken to null (public-uploads floor)', async () => {
    const c = normalizeOptions()
    expect(await c.getToken()).toBeNull()
  })
})

describe('parseElementConfig', () => {
  it('reads non-secret data-* config', () => {
    const el = document.createElement('div')
    el.setAttribute('data-endpoint', '/api/v1/uploads')
    el.setAttribute('data-product', 'image')
    el.setAttribute('data-collection', 'col-1')
    const opts = parseElementConfig(el)
    expect(opts).toEqual({ endpoint: '/api/v1/uploads', product: 'image', collectionId: 'col-1' })
  })

  it('omits unset attributes', () => {
    const el = document.createElement('div')
    expect(parseElementConfig(el)).toStrictEqual({ endpoint: undefined, product: undefined, collectionId: undefined, concurrency: undefined, maxFilesPerSession: undefined })
  })

  it('reads data-concurrency and data-max-files as numbers', () => {
    const el = document.createElement('div')
    el.setAttribute('data-concurrency', '8')
    el.setAttribute('data-max-files', '25')
    const opts = parseElementConfig(el)
    expect(opts.concurrency).toBe(8)
    expect(opts.maxFilesPerSession).toBe(25)
  })
})
