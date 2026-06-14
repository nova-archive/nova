import { describe, it, expect, vi } from 'vitest'
import { finalizeUpload, wireFinalize, buildTusOptions, fileMeta } from './uploader'
import { normalizeOptions } from './config'

const okResult = { cid: 'bafy123', byte_size: 5, mime_type: 'text/plain', product: 'raw', urls: { original: '/blob/bafy123', json: '/blob/bafy123.json' } }

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response
}

describe('finalizeUpload', () => {
  it('POSTs {uploadURL}/finalize with the bearer and returns the UploadResult', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    const cfg = normalizeOptions({ token: 'jwt' })
    const res = await finalizeUpload('http://h/api/v1/uploads/abc', cfg, { fetch })
    expect(res.cid).toBe('bafy123')
    expect(fetch).toHaveBeenCalledWith('http://h/api/v1/uploads/abc/finalize', {
      method: 'POST',
      headers: { Authorization: 'Bearer jwt' },
    })
  })

  it('sends no Authorization header when getToken yields null', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    await finalizeUpload('http://h/api/v1/uploads/abc', normalizeOptions(), { fetch })
    expect(fetch).toHaveBeenCalledWith('http://h/api/v1/uploads/abc/finalize', { method: 'POST', headers: {} })
  })

  it('throws a mapped WidgetError on a non-2xx finalize', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(422, { code: 'moderation_rejected' }))
    await expect(finalizeUpload('http://h/api/v1/uploads/abc', normalizeOptions(), { fetch }))
      .rejects.toEqual({ code: 'moderation_rejected', message: undefined })
  })
})

describe('wireFinalize', () => {
  function fakeEmitter() {
    let cb: ((file: unknown, resp: { uploadURL: string }) => unknown) | undefined
    return {
      on(_event: string, fn: (file: unknown, resp: { uploadURL: string }) => unknown) { cb = fn },
      emit(resp: { uploadURL: string }) { return cb!({}, resp) },
    }
  }

  it('finalizes on upload-success and calls onComplete (never on transport alone)', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    const onComplete = vi.fn()
    const cfg = normalizeOptions({ onComplete })
    const em = fakeEmitter()
    wireFinalize(em, cfg, { fetch })
    await em.emit({ uploadURL: 'http://h/api/v1/uploads/abc' })
    expect(onComplete).toHaveBeenCalledWith(okResult)
  })

  it('routes a finalize failure to onError', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(413, {}))
    const onError = vi.fn()
    const cfg = normalizeOptions({ onError })
    const em = fakeEmitter()
    wireFinalize(em, cfg, { fetch })
    await em.emit({ uploadURL: 'http://h/api/v1/uploads/abc' })
    expect(onError).toHaveBeenCalledWith({ code: 'payload_too_large', message: undefined })
  })

  it('routes a network failure (fetch rejects) to onError as upload_failed', async () => {
    const fetch = vi.fn().mockRejectedValue(new TypeError('network down'))
    const onError = vi.fn()
    const cfg = normalizeOptions({ onError })
    const em = fakeEmitter()
    wireFinalize(em, cfg, { fetch })
    await em.emit({ uploadURL: 'http://h/api/v1/uploads/abc' })
    expect(onError).toHaveBeenCalledWith({ code: 'upload_failed', message: 'network down' })
  })
})

describe('buildTusOptions', () => {
  it('sets endpoint, chunkSize and the allowed metadata fields', () => {
    const o = buildTusOptions(normalizeOptions({ endpoint: '/api/v1/uploads', chunkSize: 99 }))
    expect(o.endpoint).toBe('/api/v1/uploads')
    expect(o.chunkSize).toBe(99)
    expect(o.allowedMetaFields).toEqual(['filename', 'mime_type', 'product', 'collection_id'])
  })

  it('sets limit to cfg.concurrency (default 4)', () => {
    const o = buildTusOptions(normalizeOptions())
    expect(o.limit).toBe(4)
  })

  it('sets limit to the configured concurrency value', () => {
    const o = buildTusOptions(normalizeOptions({ concurrency: 2 }))
    expect(o.limit).toBe(2)
  })

  it('onBeforeRequest sets Authorization per request, omits it when token is null', async () => {
    const withTok = buildTusOptions(normalizeOptions({ token: 'jwt' }))
    const set = vi.fn()
    await withTok.onBeforeRequest({ setHeader: set })
    expect(set).toHaveBeenCalledWith('Authorization', 'Bearer jwt')

    const noTok = buildTusOptions(normalizeOptions())
    const set2 = vi.fn()
    await noTok.onBeforeRequest({ setHeader: set2 })
    expect(set2).not.toHaveBeenCalled()
  })
})

describe('fileMeta', () => {
  it('maps filename/mime/product and includes collection only when set', () => {
    expect(fileMeta(normalizeOptions({ product: 'image' }), { name: 'a.jpg', type: 'image/jpeg' }))
      .toEqual({ filename: 'a.jpg', mime_type: 'image/jpeg', product: 'image' })
    expect(fileMeta(normalizeOptions({ product: 'image', collectionId: 'c1' }), { name: 'a.jpg', type: 'image/jpeg' }))
      .toEqual({ filename: 'a.jpg', mime_type: 'image/jpeg', product: 'image', collection_id: 'c1' })
  })
})
