import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { ParanoidSection, type ParanoidState } from './ParanoidSection'

afterEach(() => cleanup())

const allOff: ParanoidState = {
  hardenNoIPRecording: false,
  hardenShortRetention: false,
  hardenPrivateDHT: false,
}
const allOn: ParanoidState = {
  hardenNoIPRecording: true,
  hardenShortRetention: true,
  hardenPrivateDHT: true,
}

function parent() {
  return screen.getByLabelText('Harden privacy (paranoid)') as HTMLInputElement
}

describe('ParanoidSection', () => {
  it('checking the parent sets every child on', () => {
    const onChange = vi.fn()
    render(<ParanoidSection value={allOff} onChange={onChange} />)
    fireEvent.click(parent())
    expect(onChange).toHaveBeenCalledWith(allOn)
  })

  it('checking the parent when fully on clears every child', () => {
    const onChange = vi.fn()
    render(<ParanoidSection value={allOn} onChange={onChange} />)
    fireEvent.click(parent())
    expect(onChange).toHaveBeenCalledWith(allOff)
  })

  it('parent is indeterminate when only some children are on', () => {
    render(
      <ParanoidSection value={{ ...allOn, hardenShortRetention: false }} onChange={vi.fn()} />,
    )
    expect(parent().indeterminate).toBe(true)
    expect(parent().checked).toBe(false)
    expect(screen.getByText(/partial/i)).toBeInTheDocument()
  })

  it('shows a consequence warning when a protection is relaxed', () => {
    render(<ParanoidSection value={allOff} onChange={vi.fn()} />)
    expect(screen.getByText(/store the client's source IP/i)).toBeInTheDocument()
    expect(screen.getByText(/kept 30 days/i)).toBeInTheDocument()
    expect(screen.getByText(/advertise stored CIDs publicly/i)).toBeInTheDocument()
  })

  it('drops the 30-day retention warning when IP recording is hardened off', () => {
    // Not recording → retention is moot; show the neutral note, not the warn.
    render(
      <ParanoidSection
        value={{ hardenNoIPRecording: true, hardenShortRetention: false, hardenPrivateDHT: true }}
        onChange={vi.fn()}
      />,
    )
    expect(screen.queryByText(/kept 30 days/i)).toBeNull()
    expect(screen.getByText(/Only applies while IP recording is on/i)).toBeInTheDocument()
  })

  it('renders the two read-only info rows', () => {
    render(<ParanoidSection value={allOn} onChange={vi.fn()} />)
    expect(screen.getByText(/No outbound webhooks/i)).toBeInTheDocument()
    expect(screen.getByText(/Metrics stay loopback-only/i)).toBeInTheDocument()
  })
})
