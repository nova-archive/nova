import { describe, expect, it } from 'vitest'
import { buildBackupContent } from './backup'

describe('buildBackupContent', () => {
  it('contains both the master key hex and the fingerprint', () => {
    const hex = '00112233445566778899aabbccddeeff'
    const fp = '0011223344556677'
    const out = buildBackupContent(hex, fp)
    expect(out).toContain(hex)
    expect(out).toContain(fp)
  })
})
