import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PublicUploadsSection, publicUploadsBlocksSave } from './PublicUploadsSection'
import { draftFromConfig } from './mergePatch'

afterEach(() => cleanup())

describe('PublicUploadsSection', () => {
  it('shows the ToS-required warning when enabled with empty ToS', () => {
    render(
      <PublicUploadsSection
        draft={{ ...draftFromConfig({}), publicUploads: true, tosUrl: '' }}
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByText(/required when public uploads are enabled/)).toBeInTheDocument()
  })

  it('publicUploadsBlocksSave true only when enabled and ToS empty', () => {
    const d = draftFromConfig({})
    expect(publicUploadsBlocksSave({ ...d, publicUploads: true, tosUrl: '' })).toBe(true)
    expect(publicUploadsBlocksSave({ ...d, publicUploads: true, tosUrl: 'https://x' })).toBe(false)
    expect(publicUploadsBlocksSave({ ...d, publicUploads: false, tosUrl: '' })).toBe(false)
  })
})
