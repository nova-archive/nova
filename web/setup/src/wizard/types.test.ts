import { describe, expect, it } from 'vitest'
import { initialForm, toAnswers } from './types'

describe('toAnswers privacy mapping', () => {
  it('default form preserves the M0.2 default posture (record IPs, 30-day, private DHT, paranoid off)', () => {
    const a = toAnswers(initialForm)
    expect(a.record_source_ip).toBe(true) // not hardened → record
    expect(a.source_ip_retention_days).toBe(30)
    expect(a.public_ipfs_dht).toBe(false) // keep-private defaults ON → DHT stays private
    expect(a.paranoid).toBe(false)
  })

  it('fully hardened form → explicit protective values + paranoid true', () => {
    const a = toAnswers({
      ...initialForm,
      hardenNoIPRecording: true,
      hardenShortRetention: true,
      hardenPrivateDHT: true,
    })
    expect(a.record_source_ip).toBe(false)
    expect(a.source_ip_retention_days).toBe(1)
    expect(a.public_ipfs_dht).toBe(false)
    expect(a.paranoid).toBe(true)
  })

  it('relaxing DHT (uncheck keep-private) → public_ipfs_dht true, paranoid false', () => {
    const a = toAnswers({ ...initialForm, hardenPrivateDHT: false })
    expect(a.public_ipfs_dht).toBe(true)
    expect(a.paranoid).toBe(false)
  })

  it('hardening only IP recording → record false, retention still 30, paranoid false', () => {
    const a = toAnswers({ ...initialForm, hardenNoIPRecording: true })
    expect(a.record_source_ip).toBe(false)
    expect(a.source_ip_retention_days).toBe(30)
    expect(a.paranoid).toBe(false)
  })
})
