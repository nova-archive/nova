import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ParanoidSection } from './ParanoidSection'
import { draftFromConfig, type SettingsDraft } from './mergePatch'

afterEach(() => cleanup())

const hardened: SettingsDraft = {
  ...draftFromConfig({}),
  hardenNoIPRecording: true,
  retentionDays: 1,
  hardenPrivateDHT: true,
}

function setup(over: Partial<Parameters<typeof ParanoidSection>[0]> = {}) {
  const onChange = vi.fn()
  render(
    <ParanoidSection
      draft={hardened}
      loadedRetentionDays={1}
      webhooksConfigured={false}
      privacyWarnings={[]}
      onChange={onChange}
      {...over}
    />,
  )
  return { onChange }
}

describe('ParanoidSection', () => {
  it('parent is checked when all children protective and no webhooks', () => {
    setup()
    const parent = screen.getByLabelText('Harden privacy (paranoid)') as HTMLInputElement
    expect(parent.checked).toBe(true)
    expect(parent.indeterminate).toBe(false)
  })

  it('parent is indeterminate when webhooks are configured, even if children hardened', () => {
    setup({ webhooksConfigured: true })
    const parent = screen.getByLabelText('Harden privacy (paranoid)') as HTMLInputElement
    expect(parent.checked).toBe(false)
    expect(parent.indeterminate).toBe(true)
    expect(screen.getByText(/clear via/i)).toBeInTheDocument()
  })

  it('unchecking the retention toggle restores the loaded >1 value, not 30', () => {
    const { onChange } = setup({ loadedRetentionDays: 7 })
    fireEvent.click(screen.getByLabelText('Keep IP logs 1 day, not 30'))
    expect(onChange).toHaveBeenCalledWith({ retentionDays: 7 })
  })

  it('renders privacy warnings as a banner', () => {
    setup({ privacyWarnings: ['paranoid on but webhooks configured'] })
    expect(screen.getByText(/webhooks configured/)).toBeInTheDocument()
  })
})
