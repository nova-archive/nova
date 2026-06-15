import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const getConfig = vi.fn()
const patchConfig = vi.fn()
let role = 'operator'

vi.mock('../auth/AuthProvider', () => ({
  useAuth: () => ({ api: { getConfig, patchConfig }, user: { role } }),
}))
vi.mock('../ui/toast', () => ({ useToast: () => ({ ok: vi.fn(), err: vi.fn() }) }))

import { SettingsScreen } from './Settings'
import { ApiError } from '../api/client'

function renderScreen() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <SettingsScreen />
    </QueryClientProvider>,
  )
}

const snapshot = {
  version: 5,
  config: { uploads: { max_upload_size_bytes: 52428800, limits: { max_files_per_session: 100 } } },
  privacy_warnings: [],
  fields: {},
}

afterEach(() => cleanup())
beforeEach(() => {
  role = 'operator'
  getConfig.mockReset().mockResolvedValue(snapshot)
  patchConfig.mockReset()
})

describe('SettingsScreen', () => {
  it('skips the query and shows operator-only notice for non-operators', async () => {
    role = 'viewer'
    renderScreen()
    expect(await screen.findByText('Settings are operator-only.')).toBeInTheDocument()
    expect(getConfig).not.toHaveBeenCalled()
  })

  it('saves a changed live field with If-Match and shows the restart banner', async () => {
    patchConfig.mockResolvedValue({
      ...snapshot,
      version: 6,
      restart_required: ['coordinator.record_source_ip'],
    })
    renderScreen()
    const input = await screen.findByLabelText('Max files per session')
    fireEvent.change(input, { target: { value: '50' } })
    fireEvent.click(screen.getByText('Save changes'))
    await waitFor(() => expect(patchConfig).toHaveBeenCalled())
    expect(patchConfig).toHaveBeenCalledWith({ uploads: { limits: { max_files_per_session: 50 } } }, 5)
    expect(await screen.findByText(/restart is required for/)).toBeInTheDocument()
  })

  it('surfaces a 409 as a conflict banner', async () => {
    patchConfig.mockRejectedValue(new ApiError(409, 'config_conflict', 'stale'))
    renderScreen()
    const input = await screen.findByLabelText('Max files per session')
    fireEvent.change(input, { target: { value: '50' } })
    fireEvent.click(screen.getByText('Save changes'))
    expect(await screen.findByText(/changed since you opened/)).toBeInTheDocument()
  })
})
