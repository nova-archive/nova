import { describe, expect, it } from 'vitest'
import {
  buildConfigPatch,
  deriveParanoid,
  draftFromConfig,
  normalizeOrigin,
  type SettingsDraft,
} from './mergePatch'

const base: SettingsDraft = {
  hardenNoIPRecording: false,
  retentionDays: 30,
  hardenPrivateDHT: true,
  corsEnabled: false,
  corsOrigins: [],
  maxUploadSizeBytes: 52428800,
  maxConcurrentAssembly: 4,
  maxConcurrentGlobal: 16,
  maxConcurrentPerSession: 4,
  maxFilesPerSession: 100,
  publicUploads: false,
  tosUrl: '',
}

describe('draftFromConfig', () => {
  it('reads the curated leaves and treats absent record_source_ip as not-hardened', () => {
    const d = draftFromConfig({ coordinator: { public_ipfs_dht: false }, uploads: {} })
    expect(d.hardenNoIPRecording).toBe(false)
    expect(d.hardenPrivateDHT).toBe(true)
    expect(d.retentionDays).toBe(30)
  })
})

describe('deriveParanoid', () => {
  it('is true only when all children protective and no webhooks', () => {
    const hardened = { ...base, hardenNoIPRecording: true, retentionDays: 1, hardenPrivateDHT: true }
    expect(deriveParanoid(hardened, false)).toBe(true)
    expect(deriveParanoid(hardened, true)).toBe(false) // webhooks present
    expect(deriveParanoid({ ...hardened, retentionDays: 7 }, false)).toBe(false)
  })
})

describe('buildConfigPatch', () => {
  it('emits nothing when unchanged', () => {
    expect(buildConfigPatch(base, base, false)).toEqual({})
  })

  it('emits only the changed leaf, never null', () => {
    const patch = buildConfigPatch({ ...base, maxUploadSizeBytes: 10 }, base, false)
    expect(patch).toEqual({ uploads: { max_upload_size_bytes: 10 } })
  })

  it('does not clobber a hand-edited 7-day retention when only DHT changes', () => {
    const loaded = { ...base, retentionDays: 7 }
    const patch = buildConfigPatch({ ...loaded, hardenPrivateDHT: false }, loaded, false)
    expect(patch).toEqual({
      coordinator: { public_ipfs_dht: true },
      auth: { paranoid: false },
    })
    expect('source_ip_retention_days' in patch).toBe(false)
  })

  it('emits derived paranoid when a privacy child changes', () => {
    const loaded = { ...base, hardenNoIPRecording: false, retentionDays: 1, hardenPrivateDHT: true }
    const patch = buildConfigPatch({ ...loaded, hardenNoIPRecording: true }, loaded, false)
    expect(patch).toEqual({
      coordinator: { record_source_ip: false },
      auth: { paranoid: true },
    })
  })
})

describe('normalizeOrigin', () => {
  it('strips path/trailing slash to a bare origin', () => {
    expect(normalizeOrigin('https://app.example.com/')).toBe('https://app.example.com')
    expect(normalizeOrigin('https://app.example.com/upload?x=1')).toBe('https://app.example.com')
  })
  it('rejects non-http(s) and garbage', () => {
    expect(normalizeOrigin('ftp://x')).toBeNull()
    expect(normalizeOrigin('not a url')).toBeNull()
    expect(normalizeOrigin('')).toBeNull()
  })
})
