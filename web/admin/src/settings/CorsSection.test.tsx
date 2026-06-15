import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CorsSection, corsBlocksSave } from './CorsSection'
import { draftFromConfig, type SettingsDraft } from './mergePatch'

afterEach(() => cleanup())

function setup(over: Partial<SettingsDraft> = {}) {
  const onChange = vi.fn()
  const draft = { ...draftFromConfig({}), ...over }
  render(<CorsSection draft={draft} onChange={onChange} />)
  return { onChange }
}

describe('CorsSection', () => {
  it('normalizes an entered origin on add', () => {
    const { onChange } = setup()
    fireEvent.change(screen.getByPlaceholderText('https://app.example.com'), {
      target: { value: 'https://app.example.com/upload' },
    })
    fireEvent.click(screen.getByText('+ Add origin'))
    expect(onChange).toHaveBeenCalledWith({ corsOrigins: ['https://app.example.com'] })
  })

  it('rejects a non-origin with an inline error', () => {
    const { onChange } = setup()
    fireEvent.change(screen.getByPlaceholderText('https://app.example.com'), {
      target: { value: 'not a url' },
    })
    fireEvent.click(screen.getByText('+ Add origin'))
    expect(onChange).not.toHaveBeenCalled()
    expect(screen.getByText(/No path or trailing slash/)).toBeInTheDocument()
  })

  it('removes an origin', () => {
    const { onChange } = setup({ corsEnabled: true, corsOrigins: ['https://a.example'] })
    fireEvent.click(screen.getByLabelText('Remove https://a.example'))
    expect(onChange).toHaveBeenCalledWith({ corsOrigins: [] })
  })

  it('warns when enabled with no origins', () => {
    setup({ corsEnabled: true, corsOrigins: [] })
    expect(screen.getByText(/match nothing/)).toBeInTheDocument()
  })

  it('corsBlocksSave true only when enabled and empty', () => {
    expect(corsBlocksSave({ ...draftFromConfig({}), corsEnabled: true, corsOrigins: [] })).toBe(true)
    expect(
      corsBlocksSave({ ...draftFromConfig({}), corsEnabled: true, corsOrigins: ['https://a'] }),
    ).toBe(false)
  })
})
