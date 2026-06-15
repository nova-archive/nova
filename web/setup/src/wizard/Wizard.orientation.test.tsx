import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { Wizard } from './Wizard'
import type { SetupApi } from '../api/client'

afterEach(() => cleanup())

const FINGERPRINT = '0011223344556677'
const HEX = '00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'

function mockApi(): SetupApi {
  return {
    state: vi.fn(async () => ({ bootstrap_complete: false })),
    generateMasterKey: vi.fn(async () => ({ master_key_hex: HEX, fingerprint: FINGERPRINT })),
    submitAnswers: vi.fn(async () => ({ ok: true })),
    commit: vi.fn(async () => ({ ok: true })),
  }
}

async function clickNext() {
  fireEvent.click(screen.getByRole('button', { name: /^(Next|Submit|Commit & go live)$/ }))
  // Let any awaited submit/commit promise settle.
  await Promise.resolve()
  await Promise.resolve()
}

describe('wizard drives to orientation', () => {
  it('shows the widget snippet and /admin after commit', async () => {
    render(<Wizard api={mockApi()} />)

    // 1 · Bootstrap token (gates Next on a non-empty token; mock ignores value).
    fireEvent.change(screen.getByLabelText('Bootstrap token'), { target: { value: 'tok' } })
    await clickNext()

    // 2 · Welcome
    fireEvent.change(screen.getByLabelText('Hostname'), { target: { value: 'nova.example.org' } })
    fireEvent.change(screen.getByLabelText('Contact email'), {
      target: { value: 'op@example.org' },
    })
    await clickNext()

    // 3 · Master key — satisfy the readback gate.
    await screen.findByTestId('fingerprint')
    fireEvent.change(screen.getByLabelText('Confirm fingerprint'), {
      target: { value: FINGERPRINT },
    })
    await clickNext()

    // 4 · Admin user
    fireEvent.change(screen.getByLabelText('Admin email'), { target: { value: 'op@example.org' } })
    fireEvent.change(screen.getByLabelText('Admin password'), {
      target: { value: 'a-very-strong-password' },
    })
    await clickNext()

    // 5 · TLS (default dev-self-signed is valid)
    await clickNext()

    // 6 · Public uploads (off → no tos_url required)
    await clickNext()

    // 7 · Privacy & hardening
    await clickNext()

    // 8 · Review → submitAnswers
    await screen.findByText('Review')
    await clickNext()

    // 9 · Commit → commit
    await screen.findByText('Commit')
    await clickNext()

    // 10 · Orientation
    await screen.findByText('You’re live')
    const snippet = screen.getByTestId('widget-snippet')
    expect(snippet).toHaveTextContent('nova-upload-widget.js')
    expect(screen.getByRole('link', { name: '/admin' })).toHaveAttribute('href', '/admin')
  })
})
