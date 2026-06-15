import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import { EffectiveConfigViewer } from './EffectiveConfigViewer'
import { CURATED_FIELDS, effectFor } from './registry'

afterEach(() => cleanup())

describe('EffectiveConfigViewer', () => {
  it('renders leaf rows with effect + env-shadow once expanded', () => {
    render(
      <EffectiveConfigViewer
        config={{ uploads: { cors: { enabled: true } }, tos_url: 'https://x' }}
        fields={{
          'uploads.cors.enabled': { effect: 'live', source: 'yaml' },
          tos_url: { effect: 'restart', source: 'env', shadowed_by_env: true },
        }}
      />,
    )
    fireEvent.click(screen.getByRole('button', { name: /Effective config/ }))
    expect(screen.getByText('uploads.cors.enabled')).toBeInTheDocument()
    expect(screen.getByText('env-shadowed')).toBeInTheDocument()
  })
})

describe('registry effectFor', () => {
  it('prefers live fields metadata over the static fallback', () => {
    expect(
      effectFor('uploads.cors.enabled', { 'uploads.cors.enabled': { effect: 'restart', source: 'yaml' } }),
    ).toBe('restart')
    expect(effectFor('uploads.cors.enabled', {})).toBe('live') // fallback to registry
  })

  it('every curated field has a known effect', () => {
    expect(CURATED_FIELDS.length).toBeGreaterThan(0)
    for (const f of CURATED_FIELDS) expect(['live', 'restart', 'env-only-inert']).toContain(f.effect)
  })
})
