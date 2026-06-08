import { describe, it, expect, vi, beforeEach } from 'vitest'

const { close, buildUppy } = vi.hoisted(() => {
  const close = vi.fn()
  const buildUppy = vi.fn(() => ({ close }))
  return { close, buildUppy }
})
vi.mock('./widget', () => ({ buildUppy }))

import { mount, mountAll, autoBootstrap } from './index'

beforeEach(() => {
  document.body.innerHTML = ''
  buildUppy.mockClear()
  close.mockClear()
})

describe('mount', () => {
  it('builds one instance and returns a handle', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    const handle = mount(el)
    expect(buildUppy).toHaveBeenCalledTimes(1)
    expect(typeof handle.destroy).toBe('function')
  })

  it('resolves a string selector', () => {
    document.body.innerHTML = '<div id="up"></div>'
    mount('#up')
    expect(buildUppy).toHaveBeenCalledTimes(1)
  })

  it('throws when the target is missing', () => {
    expect(() => mount('#nope')).toThrow(/target not found/)
  })

  it('double-mounting the same element returns the existing handle (no second instance)', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    const h1 = mount(el)
    const h2 = mount(el)
    expect(h1).toBe(h2)
    expect(buildUppy).toHaveBeenCalledTimes(1)
  })

  it('destroy() closes the instance and allows a remount', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    mount(el).destroy()
    expect(close).toHaveBeenCalledTimes(1)
    mount(el)
    expect(buildUppy).toHaveBeenCalledTimes(2)
  })
})

describe('mountAll', () => {
  it('mounts only [data-nova-upload-widget] elements', () => {
    document.body.innerHTML =
      '<div data-nova-upload-widget data-product="image"></div><div></div><div data-nova-upload-widget></div>'
    const handles = mountAll(document)
    expect(handles).toHaveLength(2)
    expect(buildUppy).toHaveBeenCalledTimes(2)
  })

  it('passes parsed data-* config through to buildUppy', () => {
    document.body.innerHTML = '<div data-nova-upload-widget data-product="image" data-collection="c1"></div>'
    mountAll(document)
    const cfg = (buildUppy.mock.calls[0] as unknown[])[1] as { product: string; collectionId?: string }
    expect(cfg.product).toBe('image')
    expect(cfg.collectionId).toBe('c1')
  })
})

describe('autoBootstrap', () => {
  it('mounts marked elements immediately when the DOM is ready', () => {
    document.body.innerHTML = '<div data-nova-upload-widget data-product="image"></div>'
    autoBootstrap()
    expect(buildUppy).toHaveBeenCalledTimes(1)
  })

  it('defers to DOMContentLoaded while the document is still loading', () => {
    const addSpy = vi.spyOn(document, 'addEventListener')
    Object.defineProperty(document, 'readyState', { configurable: true, get: () => 'loading' })
    try {
      document.body.innerHTML = '<div data-nova-upload-widget></div>'
      autoBootstrap()
      expect(addSpy).toHaveBeenCalledWith('DOMContentLoaded', expect.any(Function), { once: true })
      expect(buildUppy).not.toHaveBeenCalled()
    } finally {
      delete (document as unknown as { readyState?: unknown }).readyState
      addSpy.mockRestore()
    }
  })
})
