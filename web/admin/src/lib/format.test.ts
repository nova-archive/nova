import { describe, expect, it } from 'vitest'
import { formatBytes, shortCid, titleCase } from './format'

describe('format', () => {
  it('shortCid abbreviates long ids and keeps short ones', () => {
    expect(shortCid('short')).toBe('short')
    const long = 'bafybeigdyrabcdefghijklmnopqrstuvwxyz0123456789'
    expect(shortCid(long)).toContain('…')
    expect(shortCid(long).startsWith('bafybeigdy')).toBe(true)
  })

  it('formatBytes scales units', () => {
    expect(formatBytes(512)).toBe('512 B')
    expect(formatBytes(2048)).toBe('2.0 KB')
    expect(formatBytes(5 * 1024 * 1024)).toBe('5.0 MB')
  })

  it('titleCase humanizes snake_case', () => {
    expect(titleCase('blob.soft_deleted')).toBe('Blob.Soft Deleted')
  })
})
