import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Wizard } from './Wizard'
import type { SetupApi } from '../api/client'

afterEach(() => cleanup())

const FINGERPRINT = '0011223344556677'
const HEX = '00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'

// A fully-mocked /setup/* client. generateMasterKey resolves with a known
// fingerprint so we can drive the readback gate deterministically.
function mockApi(): SetupApi {
  return {
    state: vi.fn(async () => ({ bootstrap_complete: false })),
    generateMasterKey: vi.fn(async () => ({ master_key_hex: HEX, fingerprint: FINGERPRINT })),
    submitAnswers: vi.fn(async () => ({ ok: true })),
    commit: vi.fn(async () => ({ ok: true })),
  }
}

describe('master-key readback gate', () => {
  it('disables Next until the typed value equals the fingerprint', async () => {
    render(<Wizard api={mockApi()} />)

    // Welcome step needs hostname + contact email to advance.
    fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'nova.example.org' } })
    fireEvent.change(screen.getByLabelText('Contact email'), {
      target: { value: 'op@example.org' },
    })
    fireEvent.click(screen.getByRole('button', { name: 'Next' }))

    // Master-key step: await the async generateMasterKey resolving.
    await screen.findByTestId('fingerprint')
    expect(screen.getByTestId('fingerprint')).toHaveTextContent(FINGERPRINT)

    const next = screen.getByRole('button', { name: 'Next' })
    const readback = screen.getByLabelText('Confirm fingerprint')

    // Disabled with no input and with a wrong value.
    expect(next).toBeDisabled()
    fireEvent.change(readback, { target: { value: 'wrong-fingerprint' } })
    expect(next).toBeDisabled()

    // Enabled once the typed value (trimmed) equals the returned fingerprint.
    fireEvent.change(readback, { target: { value: `  ${FINGERPRINT}  ` } })
    expect(next).toBeEnabled()
  })
})
