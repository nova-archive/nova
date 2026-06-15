import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { UploadLimitsSection } from './UploadLimitsSection'
import { draftFromConfig } from './mergePatch'

afterEach(() => cleanup())

describe('UploadLimitsSection', () => {
  it('emits the numeric change for a field', () => {
    const onChange = vi.fn()
    render(
      <UploadLimitsSection
        draft={{ ...draftFromConfig({}), maxFilesPerSession: 100 }}
        onChange={onChange}
      />,
    )
    fireEvent.change(screen.getByLabelText('Max files per session'), { target: { value: '50' } })
    expect(onChange).toHaveBeenCalledWith({ maxFilesPerSession: 50 })
  })

  it('shows a human-readable hint for the byte field', () => {
    render(
      <UploadLimitsSection
        draft={{ ...draftFromConfig({}), maxUploadSizeBytes: 52428800 }}
        onChange={vi.fn()}
      />,
    )
    expect(screen.getByText('50 MB')).toBeInTheDocument()
  })
})
